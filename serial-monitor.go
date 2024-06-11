package main

import (
	"bytes"
	"log"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/tarm/serial"
)

// Конфигурация для WebSocket с увеличенными буферами чтения и записи
var (
	websocketUpgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}
	// clients хранит подключенных клиентов WebSocket
	clients = make(map[*websocket.Conn]bool)
	// Канал для отправки данных из последовательного порта подключенным клиентам
	broadcast = make(chan string)
	// Канал для отправки сообщений на последовательный порт
	serialWriteChan = make(chan string)
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

ClientLoop:
	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("Ошибка чтения сообщения: %v", err)
			} else {
				log.Println("Клиент отключён")
			}
			break ClientLoop // Используем метку для выхода из внешнего цикла
		}

		log.Printf("Отправка команды на последовательный порт: %s", msg)

		// Отправка команды на последовательный порт
		serialWriteChan <- string(msg)

		// Чтение ответа от последовательного порта
		select {
		case response := <-broadcast:
			log.Printf("Получен ответ от последовательного порта: %s", response)
			// Отправка ответа клиенту WebSocket
			err := ws.WriteMessage(websocket.TextMessage, []byte(response))
			if err != nil {
				log.Printf("Ошибка отправки ответа клиенту: %v", err)
				break ClientLoop // Используем метку для выхода из внешнего цикла
			}
		case <-time.After(5 * time.Second):
			log.Println("Не удалось получить ответ от последовательного порта в течение 5 секунд")
			break ClientLoop // Используем метку для выхода из внешнего цикла
		}
	}

	delete(clients, ws)
}

func manageSerialConnection() {
	for {
		err := readFromSerial()
		if err != nil {
			log.Printf("Ошибка последовательного порта: %v", err)
			log.Println("Попытка переподключения к последовательному порту через 5 секунд...")
			time.Sleep(5 * time.Second)
		}
	}
}

func readFromSerial() error {
	c := &serial.Config{Name: "COM6", Baud: 9600}
	s, err := serial.OpenPort(c)
	if err != nil {
		log.Printf("Не удалось открыть последовательный порт: %v", err)
		return err
	}
	defer s.Close()

	buf := make([]byte, 128)
	var messageBuffer bytes.Buffer

	for {
		n, err := s.Read(buf)
		if err != nil {
			log.Printf("Не удалось прочитать из последовательного порта: %v", err)
			return err
		}

		data := buf[:n]
		if !utf8.Valid(data) {
			log.Printf("Недействительная строка UTF-8: %x", data)
			continue
		}

		messageBuffer.Write(data)
		for {
			if i := bytes.IndexByte(messageBuffer.Bytes(), '\n'); i >= 0 {
				message := messageBuffer.Next(i + 1)
				broadcast <- string(message)
			} else {
				break
			}
		}
	}
}

func writeToSerial() {
	c := &serial.Config{Name: "COM6", Baud: 9600}
	s, err := serial.OpenPort(c)
	if err != nil {
		log.Fatalf("Не удалось открыть последовательный порт: %v", err)
	}
	defer s.Close()

	for {
		msg := <-serialWriteChan
		_, err := s.Write([]byte(msg))
		if err != nil {
			log.Printf("Ошибка записи в последовательный порт: %v", err)
		} else {
			log.Printf("Записано в последовательный порт: %s", msg)
		}
	}
}

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
