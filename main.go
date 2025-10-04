package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// Message struct for MongoDB
type Message struct {
	RoomID    string    `bson:"room_id" json:"roomId"`
	UserID    string    `bson:"user_id" json:"userId"`
	UserName  string    `bson:"user_name" json:"userName"`
	Message   string    `bson:"message" json:"message"`
	Timestamp time.Time `bson:"timestamp" json:"timestamp"`
}

// Client represents a WebSocket connection
type Client struct {
	conn     *websocket.Conn
	send     chan []byte
	room     *Room
	userID   string
	userName string
}

// Room represents a chat room
type Room struct {
	id         string
	clients    map[*Client]bool
	broadcast  chan []byte
	register   chan *Client
	unregister chan *Client
	mu         sync.Mutex
	manager    *RoomManager
}

// RoomManager manages all rooms
type RoomManager struct {
	rooms       map[string]*Room
	mu          sync.RWMutex
	mongoClient *mongo.Client
	db          *mongo.Database
}

var roomManager *RoomManager

func newRoomManager(mongoClient *mongo.Client) *RoomManager {
	return &RoomManager{
		rooms:       make(map[string]*Room),
		mongoClient: mongoClient,
		db:          mongoClient.Database("chatapp"),
	}
}

func (rm *RoomManager) getOrCreateRoom(roomID string) *Room {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	if room, exists := rm.rooms[roomID]; exists {
		return room
	}

	room := &Room{
		id:         roomID,
		clients:    make(map[*Client]bool),
		broadcast:  make(chan []byte, 256),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		manager:    rm,
	}

	rm.rooms[roomID] = room
	go room.run()

	log.Printf("Room created: %s", roomID)
	return room
}

func (rm *RoomManager) deleteRoom(roomID string) {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	delete(rm.rooms, roomID)
	log.Printf("Room deleted: %s", roomID)
}

func (rm *RoomManager) saveMessage(msg Message) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	collection := rm.db.Collection("messages")
	_, err := collection.InsertOne(ctx, msg)
	return err
}

func (rm *RoomManager) getMessageHistory(roomID string) ([]Message, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	collection := rm.db.Collection("messages")
	opts := options.Find().SetSort(bson.D{{Key: "timestamp", Value: 1}})

	cursor, err := collection.Find(ctx, bson.M{"room_id": roomID}, opts)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var messages []Message
	if err = cursor.All(ctx, &messages); err != nil {
		return nil, err
	}

	return messages, nil
}

func (r *Room) run() {
	for {
		select {
		case client := <-r.register:
			r.mu.Lock()
			r.clients[client] = true
			clientCount := len(r.clients)
			r.mu.Unlock()
			log.Printf("Client joined room %s. Total clients: %d", r.id, clientCount)

		case client := <-r.unregister:
			r.mu.Lock()
			if _, ok := r.clients[client]; ok {
				delete(r.clients, client)
				close(client.send)
				clientCount := len(r.clients)
				r.mu.Unlock()

				log.Printf("Client left room %s. Total clients: %d", r.id, clientCount)

				// Delete room if empty
				if clientCount == 0 {
					r.manager.deleteRoom(r.id)
					return // Exit the goroutine
				}
			} else {
				r.mu.Unlock()
			}

		case message := <-r.broadcast:
			r.mu.Lock()
			for client := range r.clients {
				select {
				case client.send <- message:
				default:
					close(client.send)
					delete(r.clients, client)
				}
			}
			r.mu.Unlock()
		}
	}
}

func (c *Client) readPump() {
	defer func() {
		c.room.unregister <- c
		c.conn.Close()
	}()

	for {
		_, msgBytes, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("error: %v", err)
			}
			break
		}

		// Parse incoming message
		var incomingMsg map[string]interface{}
		if err := json.Unmarshal(msgBytes, &incomingMsg); err != nil {
			log.Printf("Error parsing message: %v", err)
			continue
		}

		// Create message for storage
		msg := Message{
			RoomID:    c.room.id,
			UserID:    incomingMsg["userId"].(string),
			UserName:  incomingMsg["userName"].(string),
			Message:   incomingMsg["message"].(string),
			Timestamp: time.Now(),
		}

		// Save to MongoDB
		if err := roomManager.saveMessage(msg); err != nil {
			log.Printf("Error saving message: %v", err)
		}

		// Broadcast to room
		c.room.broadcast <- msgBytes
	}
}

func (c *Client) writePump() {
	defer c.conn.Close()

	for message := range c.send {
		err := c.conn.WriteMessage(websocket.TextMessage, message)
		if err != nil {
			log.Printf("Write error: %v", err)
			return
		}
	}
}

func handleWebSocket(c *gin.Context) {
	roomID := c.Param("roomId")
	if roomID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Room ID required"})
		return
	}

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}

	room := roomManager.getOrCreateRoom(roomID)

	client := &Client{
		conn: conn,
		send: make(chan []byte, 256),
		room: room,
	}

	room.register <- client

	// Send message history to the new client
	go func() {
		messages, err := roomManager.getMessageHistory(roomID)
		if err != nil {
			log.Printf("Error fetching history: %v", err)
			return
		}

		for _, msg := range messages {
			msgBytes, _ := json.Marshal(msg)
			client.send <- msgBytes
		}
	}()

	go client.writePump()
	go client.readPump()
}

func main() {
	// Connect to MongoDB
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	mongoURI := "mongodb+srv://suryakanta:5p17Bkv97rw9y9FX@cluster0.dtft09e.mongodb.net/?retryWrites=true&w=majority"

	clientOptions := options.Client().
		ApplyURI(mongoURI).
		SetServerAPIOptions(options.ServerAPI(options.ServerAPIVersion1))

	mongoClient, err := mongo.Connect(ctx, clientOptions)
	if err != nil {
		log.Fatal("MongoDB connection error:", err)
	}

	defer func() {
		if err := mongoClient.Disconnect(context.Background()); err != nil {
			log.Printf("Error disconnecting from MongoDB: %v", err)
		}
	}()

	// Verify connection
	if err := mongoClient.Ping(ctx, nil); err != nil {
		log.Fatal("MongoDB ping error:", err)
	}
	log.Println("Connected to MongoDB!")

	// Initialize room manager
	roomManager = newRoomManager(mongoClient)

	r := gin.Default()

	r.LoadHTMLGlob("templates/*")

	// Serve chat page with room ID
	r.GET("/room/:roomId", func(c *gin.Context) {
		c.HTML(http.StatusOK, "room.html", gin.H{
			"roomId": c.Param("roomId"),
		})
	})

	// WebSocket endpoint for specific room
	r.GET("/ws/:roomId", handleWebSocket)

	// API to get active rooms
	r.GET("/api/rooms", func(c *gin.Context) {
		roomManager.mu.RLock()
		defer roomManager.mu.RUnlock()

		rooms := make([]gin.H, 0)
		for roomID, room := range roomManager.rooms {
			room.mu.Lock()
			clientCount := len(room.clients)
			room.mu.Unlock()

			rooms = append(rooms, gin.H{
				"roomId":      roomID,
				"clientCount": clientCount,
			})
		}
		c.JSON(http.StatusOK, gin.H{"rooms": rooms})
	})

	// Home page
	r.GET("/", func(c *gin.Context) {
		c.HTML(http.StatusOK, "index.html", nil)
	})

	log.Println("Server starting on :8080")
	if err := r.Run(":8080"); err != nil {
		log.Fatal("Server error:", err)
	}
}
