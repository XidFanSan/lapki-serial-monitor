package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tarm/serial"
)

// Структура для хранения настроек порта и скорости передачи
type SerialSettings struct {
	Port     string `json:"port"`
	BaudRate int    `json:"baudRate"`
}

// Конфигурация для WebSocket
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

	log.Println("Запускаем сервер :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatal("Ошибка запуска сервера: ", err)
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

	log.Println("Новый клиент подключён.")
	clients[ws] = true

	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("Ошибка чтения сообщения: %v", err)
			} else {
				log.Println("Клиент отключён.")
			}
			break
		}

		var message map[string]interface{}
		if err := json.Unmarshal(msg, &message); err != nil {
			log.Printf("Ошибка парсинга сообщения: %v", err)
			continue
		}

		processSettings(message)
	}

	delete(clients, ws)
}

// Переопределение настроек и получение команд от клиента
func processSettings(message map[string]interface{}) {
	port, portOk := message["port"]
	baudRate, baudRateOk := message["baudRate"]

	if portOk && baudRateOk {
		portStr, portValid := port.(string)
		baudRateStr, baudRateValid := baudRate.(string)

		if portValid && baudRateValid {
			baudRateInt, err := strconv.Atoi(baudRateStr)
			if err == nil {
				if currentSettings.Port != portStr || currentSettings.BaudRate != baudRateInt {
					currentSettings = SerialSettings{
						Port:     portStr,
						BaudRate: baudRateInt,
					}
					log.Printf("Изменены настройки: порт %s, скорость передачи %d", portStr, baudRateInt)
					notifyClients(fmt.Sprintf("Изменены настройки: порт %s, скорость передачи %d", portStr, baudRateInt))
					reconnectSerialPort()
				} else {
					notifyClients("Настройки порта и скорости передачи не изменились.")
				}
			} else {
				notifyClients("Ошибка преобразования скорости передачи.")
			}
		} else {
			if !portValid {
				notifyClients("Ошибка: неверный тип данных для порта.")
			}
			if !baudRateValid {
				notifyClients("Ошибка: неверный тип данных для скорости передачи.")
			}
		}
	} else if command, ok := message["command"]; ok {
		if commandStr, commandOk := command.(string); commandOk {
			serialWriteChan <- commandStr + "\n"
		} else {
			notifyClients("Ошибка: неверный тип данных для команды.")
		}
	}
}

// Функция переподключения к последовательному порту, если плата была переподключена или перезагружена
func manageSerialConnection() {
	for {
		if serialPort == nil {
			log.Println("Ошибка: последовательный порт не открыт.")
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

	// Небольшая пауза перед повторной попыткой открыть порт
	time.Sleep(1 * time.Second)

	// Открываем новое соединение
	openSerialPort()
}

// Открываем порт заново, если он был закрыт
func openSerialPort() {
	if currentSettings.Port == "" {
		notifyClients("Порт не выбран.")
		return
	}
	c := &serial.Config{Name: currentSettings.Port, Baud: currentSettings.BaudRate}
	var err error
	serialPort, err = serial.OpenPort(c)
	if err != nil {
		notifyClients("Ошибка: не удалось открыть последовательный порт. Проверьте настройки и переподключитесь к порту.")
		return
	}
	go func() {
		if err := readFromSerial(); err != nil {
			log.Printf("Ошибка при чтении из последовательного порта: %v", err)
		}
	}()
	notifyClients("Подключение к последовательному порту успешно!")
}

// Получаем ответ из последовательного порта
func readFromSerial() error {
	// Проверяем наличие порта
	if serialPort == nil {
		errMsg := "Ошибка: последовательный порт не открыт."
		notifyClients(errMsg)
		return errors.New("ошибка: последовательный порт не открыт")
	}

	reader := bufio.NewReader(serialPort)
	for {
		receivedMsg, err := reader.ReadString('\n') // Читаем до символа новой строки
		if err != nil {
			log.Printf("Ошибка при чтении из последовательного порта: %v", err)
			return err
		}

		receivedMsg = strings.TrimSpace(receivedMsg) // Удаляем пробельные символы
		if receivedMsg != "" {
			broadcast <- receivedMsg // Отправляем сообщение клиентам
			log.Println("Получен ответ из последовательного порта:", receivedMsg)
		}
		time.Sleep(100 * time.Millisecond) // Добавляем небольшую задержку
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

// Структура отправляемого сообщения клиенту
func notifyClients(message string) {
	for client := range clients {
		err := client.WriteMessage(websocket.TextMessage, []byte(message))
		if err != nil {
			log.Printf("Ошибка отправки сообщения клиенту: %v", err)
		}
	}
}
