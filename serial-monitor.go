package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/tarm/serial"
)

// Структура для хранения настроек порта и скорости передачи
type SerialSettings struct {
	Port     string `json:"port"`
	BaudRate int    `json:"baudRate"`
}

// Структура для передачи команд от клиента
type Command struct {
	Command string `json:"command"`
}

// Структура для сообщений клиенту
type ClientMessage struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Конфигурация для WebSocket с увеличенными буферами чтения и записи
var (
	websocketUpgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}
	// Хранит подключенных клиентов WebSocket
	clients = make(map[*websocket.Conn]bool)
	// Канал для отправки данных из последовательного порта подключенным клиентам
	broadcast = make(chan string)
	// Канал для отправки сообщений на последовательный порт
	serialWriteChan = make(chan string)
	// Мьютекс для синхронизации доступа к serialPort
	serialPortMutex = &sync.Mutex{}
	// Глобальная переменная для хранения текущих настроек
	currentSettings SerialSettings
	serialPort      *serial.Port
)

func main() {
	http.HandleFunc("/serialmonitor", handleConnections)

	go handleMessages()
	go manageSerialConnection()
	go writeToSerial()

	log.Println("Server started on :8080")
	err := http.ListenAndServe(":8080", nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}

// Обработчик WebSocket соединений
func handleConnections(w http.ResponseWriter, r *http.Request) {
	websocketUpgrader.CheckOrigin = func(r *http.Request) bool { return true }

	ws, err := websocketUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Ошибка обновления соединения: %v", err)
		return
	}
	defer ws.Close()

	log.Println("Новый клиент подключён")
	clients[ws] = true

	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("Ошибка чтения сообщения: %v", err)
			} else {
				log.Println("Клиент отключён")
			}
			break
		}

		var message map[string]interface{}
		if err := json.Unmarshal(msg, &message); err != nil {
			log.Printf("Ошибка парсинга сообщения: %v", err)
			continue
		}

		if port, ok := message["port"]; ok {
			if baudRate, ok := message["baudRate"]; ok {
				portStr, portOk := port.(string)
				baudRateStr, baudRateOk := baudRate.(string)
				baudRateInt, err := strconv.Atoi(baudRateStr)
				if err != nil {
					log.Printf("Ошибка преобразования скорости передачи: %v", err)
					continue
				}
				if portOk && baudRateOk {
					currentSettings = SerialSettings{
						Port:     portStr,
						BaudRate: baudRateInt,
					}
					log.Printf("Изменены настройки: порт %s, скорость передачи %d", portStr, baudRateInt)
					// Закрываем предыдущее соединение и открываем новое
					reconnectSerialPort()
				} else {
					log.Println("Ошибка: неверные типы данных для порта и скорости передачи")
				}
			}
		} else if command, ok := message["command"]; ok {
			if commandStr, commandOk := command.(string); commandOk {
				serialWriteChan <- commandStr + "\n"
			} else {
				log.Println("Ошибка: неверный тип данных для команды")
			}
		}
	}

	delete(clients, ws)
}

func manageSerialConnection() {
	for {
		if serialPort == nil {
			log.Println("Ошибка: последовательный порт не открыт")
			time.Sleep(5 * time.Second)
			continue
		}

		if err := readFromSerial(); err != nil {
			log.Printf("Ошибка последовательного порта: %v", err)
			log.Println("Попытка переподключения к последовательному порту через 5 секунд...")
			time.Sleep(5 * time.Second)

			// Переподключаемся при ошибке чтения
			reconnectSerialPort()
		}
	}
}

func reconnectSerialPort() {
	serialPortMutex.Lock()
	defer serialPortMutex.Unlock()

	if serialPort != nil {
		serialPort.Close()
	}

	// Открываем новое соединение
	openSerialPort()
}

// Открываем порт заново, если он был закрыт
func openSerialPort() {
	if currentSettings.Port == "" {
		log.Println("Порт не выбран")
		return
	}
	c := &serial.Config{Name: currentSettings.Port, Baud: currentSettings.BaudRate}
	var err error
	serialPort, err = serial.OpenPort(c)
	if err != nil {
		log.Printf("Не удалось открыть последовательный порт: %v", err)
		notifyClients("Ошибка: не удалось открыть последовательный порт. Пожалуйста, проверьте настройки и переподключитесь к порту.")
		return
	}
	go readFromSerial()
	notifyClients("Подключение к последовательному порту успешно. Начинается отправка сообщения с последовательного порта...")
}

// Структура отправляемого сообщения клиенту
func notifyClients(message string) {
	for client := range clients {
		err := client.WriteMessage(websocket.TextMessage, []byte(message))
		if err != nil {
			log.Printf("Ошибка отправки сообщения клиенту: %v", err)
		}
	}
}

// Получаем ответ из последовательного порта
func readFromSerial() error {
	// Проверяем наличие порта
	if serialPort == nil {
		errMsg := "Ошибка: последовательный порт не открыт"
		notifyClients(errMsg)
		return errors.New("ошибка: последовательный порт не открыт")
	}

	// Буфер для чтения данных из порта
	buf := make([]byte, 128)
	var messageBuffer bytes.Buffer

	for {
		n, err := serialPort.Read(buf)
		if err != nil {
			log.Printf("Не удалось прочитать из последовательного порта: %v", err)
			return err
		}

		// Обрабатываем полученные данные
		data := buf[:n]
		if !utf8.Valid(data) {
			log.Printf("Недействительная строка UTF-8: %x", data)
			continue
		}

		// Добавляем данные в буфер сообщений
		messageBuffer.Write(data)

		// Парсим сообщения из буфера
		for {
			if i := bytes.IndexByte(messageBuffer.Bytes(), '\n'); i >= 0 {
				message := messageBuffer.Next(i + 1)
				broadcast <- string(message)
				log.Println("Получен ответ из последовательного порта:", string(message))
			} else {
				break
			}
		}
	}
}

// Отправление сообщения от клиента в последовательный порт
func writeToSerial() {
	for {
		msg := <-serialWriteChan
		if serialPort == nil {
			errMsg := "Ошибка: порт не открыт. Сообщение не отправлено."
			notifyClients(errMsg)
			continue
		}

		_, err := serialPort.Write([]byte(msg))
		if err != nil {
			errMsg := "Ошибка записи в последовательный порт: " + err.Error()
			notifyClients(errMsg)
		} else {
			errMsg := "Записано в последовательный порт: " + msg
			notifyClients(errMsg)
		}
	}
}

// Отправление сообщения к клиенту
func handleMessages() {
	for {
		msg := <-broadcast
		log.Println("Отправка сообщения:", msg)

		for client := range clients {
			err := client.WriteMessage(websocket.TextMessage, []byte(msg))
			if err != nil {
				log.Printf("Ошибка записи сообщения клиенту: %v", err)
				client.Close()
				delete(clients, client)
			}
		}
	}
}
