package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
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
	// адрес на котором будет работать этот сервер
	webAddress string
)

func main() {
	flag.StringVar(&webAddress, "address", ":8080", "адрес для подключения")

	http.HandleFunc("/serialmonitor", handleConnections)

	go handleMessages()
	go manageSerialConnection()
	go writeToSerial()

	log.Println("Запускаем сервер :8080")
	log.Fatal(http.ListenAndServe(webAddress, nil))
}

// Отправление сообщения клиенту
func handleMessages() {
	for {
		msg := <-broadcast

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

	//Отправляем первоначальное сообщение о портах
	sendPortList()

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
					broadcast <- fmt.Sprintf("Изменены настройки: порт %s, скорость передачи %d", portStr, baudRateInt)
					reconnectSerialPort()
				} else {
					broadcast <- "Настройки порта и скорости передачи не изменились."
				}
			} else {
				broadcast <- "Ошибка преобразования скорости передачи."
			}
		} else {
			if !portValid {
				broadcast <- "Ошибка: неверный тип данных для порта."
			}
			if !baudRateValid {
				broadcast <- "Ошибка: неверный тип данных для скорости передачи."
			}
		}
	} else if command, ok := message["command"]; ok {
		if commandStr, commandOk := command.(string); commandOk {
			serialWriteChan <- commandStr + "\n"
		} else {
			broadcast <- "Ошибка: неверный тип данных для команды."
		}
	}
}

// Функция для переподключения, а также для обновления данных(список портов и т.д.)
func manageSerialConnection() {
	// Получаем текущий список портов
	lastPortList := getPortNames()
	for {
		// Получаем текущий список портов
		portList := getPortNames()
		if !equalPortLists(portList, lastPortList) {
			sendPortList()
			// Проверяем, если текущий порт больше недоступен, сбрасываем настройки и переподключаемся
			if !stringInSlice(currentSettings.Port, portList) {
				if len(portList) < len(lastPortList) {
					currentSettings = SerialSettings{}
					broadcast <- "Текущий порт больше не доступен. Настройки сброшены."
				}

				reconnectSerialPort()
			}
			// Обновляем последний известный список портов
			lastPortList = portList
		}
		time.Sleep(2 * time.Second)
	}
}

// Функция для проверки наличия строки в слайсе
func stringInSlice(str string, list []string) bool {
	for _, v := range list {
		if str == v {
			return true
		}
	}
	return false
}

// Функция для проверки наличия списка портов в слайсе
func equalPortLists(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
		broadcast <- "Порт не выбран."
		return
	}
	c := &serial.Config{Name: currentSettings.Port, Baud: currentSettings.BaudRate}
	var err error
	serialPort, err = serial.OpenPort(c)
	if err != nil {
		broadcast <- "Ошибка: не удалось открыть последовательный порт. Проверьте настройки и переподключитесь к порту."
		return
	}
	go func() {
		readFromSerial()
	}()
	broadcast <- fmt.Sprintf("Подключение к последовательному порту %s со скоростью %d успешно!", currentSettings.Port, currentSettings.BaudRate)
}

// Получаем ответ из последовательного порта
func readFromSerial() error {
	// Проверяем наличие порта
	if serialPort == nil {
		broadcast <- "Ошибка: последовательный порт не открыт."
		return errors.New("ошибка: последовательный порт не открыт")
	}

	reader := bufio.NewReader(serialPort)
	for {
		// Читаем до символа новой строки
		receivedMsg, err := reader.ReadString('\n')
		if err != nil {
			broadcast <- fmt.Sprintf("Ошибка при чтении из последовательного порта: %v", err)
			return err
		}
		// Удаляем пробельные символы
		receivedMsg = strings.TrimSpace(receivedMsg)
		if receivedMsg != "" {
			// Отправляем сообщение клиентам
			broadcast <- receivedMsg
		}
		// Добавляем небольшую задержку перед чтением нового сообщения
		time.Sleep(100 * time.Millisecond)
	}
}

// Отправление сообщения от клиента в последовательный порт
func writeToSerial() {
	for {
		msg := <-serialWriteChan
		if serialPort == nil {
			broadcast <- "Ошибка: порт не открыт. Сообщение не отправлено."
			continue
		}

		_, err := serialPort.Write([]byte(msg))
		if err != nil {
			broadcast <- "Ошибка записи в последовательный порт: " + err.Error()
		} else {
			broadcast <- "Отправлено на последовательный порт: " + msg
		}
	}
}

// Функция для отправки списка портов клиентам по WebSocket
func sendPortList() error {
	ports := getPortNames()
	data, err := json.Marshal(ports)
	if err != nil {
		broadcast <- fmt.Sprintf("Ошибка при маршалинге списка портов: %v", err)
		return err
	}

	message := fmt.Sprintf("Получен новый список портов: %v", ports)
	broadcast <- message
	broadcast <- string(data)

	return nil
}

// Функция для получения списка доступных портов
func getPortNames() []string {
	var ports []string

	switch {
	case isWindows():
		ports = getWindowsPortNames()
	default:
		//ports = getUnixPortNames()
	}

	return ports
}

// Функция для определения операционной системы Windows
func isWindows() bool {
	return os.PathSeparator == '\\' && os.Getenv("WINDIR") != ""
}

// Функция для получения списка COM-портов в Windows
func getWindowsPortNames() []string {
	var ports []string

	for i := 1; i <= 256; i++ {
		port := "COM" + strconv.Itoa(i)
		_, err := os.Stat(port)
		if !os.IsNotExist(err) {
			ports = append(ports, port)
		}
	}

	return ports
}
