package main

import (
	"log"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/tarm/serial"
)

// Конфигурация для WebSocket с увеличенными буферами чтения и записи
var (
	websocketUpgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}
)

// clients хранит подключенных клиентов WebSocket
var clients = make(map[*websocket.Conn]bool)

// broadcast - это канал для отправки данных из последовательного порта подключенным клиентам
var broadcast = make(chan string)

func main() {
	// Обработчик для WebSocket соединений
	http.HandleFunc("/ws", handleConnections)

	// Запуск функций для обработки сообщений и чтения с последовательного порта в отдельных горутинах
	go handleMessages()
	go readFromSerial()

	// Запуск HTTP-сервера на порту 8080
	log.Println("Server started on :8080")
	err := http.ListenAndServe(":8080", nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}

// Обработка входящего WebSocket соединения
func handleConnections(w http.ResponseWriter, r *http.Request) {
	// Проверка происхождения запросов
	websocketUpgrader.CheckOrigin = func(r *http.Request) bool { return true }

	// Обновление соединения до WebSocket
	ws, err := websocketUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer ws.Close()

	// Регистрация нового клиента
	clients[ws] = true

	// Ожидание и обработка входящих сообщений от клиента
	for {
		messageType, p, err := ws.ReadMessage()
		if err != nil {
			// Удаление клиента при ошибке
			delete(clients, ws)
			break
		}
		log.Print(messageType, p)
	}
}

// Читаем данные из последовательного порта и отправляем их в канал broadcast
func readFromSerial() {
	// Конфигурация последовательного порта
	c := &serial.Config{Name: "COM6", Baud: 9600}
	s, err := serial.OpenPort(c)
	if err != nil {
		log.Fatal(err)
	}

	buf := make([]byte, 128)
	for {
		// Чтение данных из последовательного порта
		n, err := s.Read(buf)
		if err != nil {
			log.Fatal(err)
		}
		// Отправка прочитанных данных в канал broadcast
		broadcast <- string(buf[:n])
	}
}

// Отправляем сообщения из канала broadcast всем подключенным клиентам
func handleMessages() {
	for {
		// Получение сообщения из канала broadcast
		msg := <-broadcast
		// Отправка сообщения всем клиентам
		for client := range clients {
			err := client.WriteMessage(websocket.TextMessage, []byte(msg))
			if err != nil {
				log.Printf("websocket error: %v", err)
				client.Close()
				delete(clients, client)
			}
		}
	}
}
