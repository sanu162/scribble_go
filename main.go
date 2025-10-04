package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"net/smtp"
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

// RoomInfo struct for MongoDB
type RoomInfo struct {
	RoomID      string   `bson:"room_id" json:"roomId"`
	IsPrivate   bool     `bson:"is_private" json:"isPrivate"`
	CreatorEmail string  `bson:"creator_email" json:"creatorEmail"`
	AllowedEmails []string `bson:"allowed_emails" json:"allowedEmails"`
	CreatedAt   time.Time `bson:"created_at" json:"createdAt"`
}

// OTP struct
type OTP struct {
	Email     string    `json:"email"`
	Code      string    `json:"code"`
	RoomID    string    `json:"roomId"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Client represents a WebSocket connection
type Client struct {
	conn     *websocket.Conn
	send     chan []byte
	room     *Room
	userID   string
	userName string
	email    string
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
	isPrivate  bool
}

// RoomManager manages all rooms
type RoomManager struct {
	rooms       map[string]*Room
	mu          sync.RWMutex
	mongoClient *mongo.Client
	db          *mongo.Database
	otpStore    map[string]*OTP
	otpMu       sync.RWMutex
}

var roomManager *RoomManager

// Email configuration - UPDATE THESE WITH YOUR SMTP DETAILS
const (
	smtpHost     = "smtp.gmail.com"
	smtpPort     = "587"
	senderEmail  = "your-email@gmail.com"  // Change this
	senderPassword = "your-app-password"    // Change this (use App Password for Gmail)
)

func newRoomManager(mongoClient *mongo.Client) *RoomManager {
	return &RoomManager{
		rooms:       make(map[string]*Room),
		mongoClient: mongoClient,
		db:          mongoClient.Database("chatapp"),
		otpStore:    make(map[string]*OTP),
	}
}

func (rm *RoomManager) getOrCreateRoom(roomID string, isPrivate bool) *Room {
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
		isPrivate:  isPrivate,
	}

	rm.rooms[roomID] = room
	go room.run()

	log.Printf("Room created: %s (Private: %v)", roomID, isPrivate)
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

func (rm *RoomManager) saveRoomInfo(roomInfo RoomInfo) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	collection := rm.db.Collection("rooms")
	_, err := collection.InsertOne(ctx, roomInfo)
	return err
}

func (rm *RoomManager) getRoomInfo(roomID string) (*RoomInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	collection := rm.db.Collection("rooms")
	var roomInfo RoomInfo
	err := collection.FindOne(ctx, bson.M{"room_id": roomID}).Decode(&roomInfo)
	if err != nil {
		return nil, err
	}
	return &roomInfo, nil
}

func (rm *RoomManager) addAllowedEmail(roomID, email string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	collection := rm.db.Collection("rooms")
	_, err := collection.UpdateOne(
		ctx,
		bson.M{"room_id": roomID},
		bson.M{"$addToSet": bson.M{"allowed_emails": email}},
	)
	return err
}

func generateOTP() string {
	max := big.NewInt(1000000)
	n, _ := rand.Int(rand.Reader, max)
	return fmt.Sprintf("%06d", n.Int64())
}

func sendOTPEmail(to, otp, roomID string) error {
	from := senderEmail
	password := senderPassword

	message := []byte(fmt.Sprintf("Subject: OTP for Private Room Access\r\n\r\n"+
		"Your OTP to join room '%s' is: %s\r\n\r\n"+
		"This OTP will expire in 5 minutes.\r\n\r\n"+
		"If you didn't request this, please ignore this email.", roomID, otp))

	auth := smtp.PlainAuth("", from, password, smtpHost)
	err := smtp.SendMail(smtpHost+":"+smtpPort, auth, from, []string{to}, message)
	return err
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
				
				if clientCount == 0 {
					r.manager.deleteRoom(r.id)
					return
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

		var incomingMsg map[string]interface{}
		if err := json.Unmarshal(msgBytes, &incomingMsg); err != nil {
			log.Printf("Error parsing message: %v", err)
			continue
		}

		msg := Message{
			RoomID:    c.room.id,
			UserID:    incomingMsg["userId"].(string),
			UserName:  incomingMsg["userName"].(string),
			Message:   incomingMsg["message"].(string),
			Timestamp: time.Now(),
		}

		if err := roomManager.saveMessage(msg); err != nil {
			log.Printf("Error saving message: %v", err)
		}

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
	email := c.Query("email") // Get email from query parameter
	
	if roomID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Room ID required"})
		return
	}

	// Check if room exists and if it's private
	roomInfo, err := roomManager.getRoomInfo(roomID)
	if err == nil && roomInfo.IsPrivate {
		// Room is private - verify email access
		if email == "" {
			log.Printf("Access denied: No email provided for private room %s", roomID)
			conn, _ := upgrader.Upgrade(c.Writer, c.Request, nil)
			conn.WriteJSON(map[string]string{"error": "access_denied", "message": "Email required for private room"})
			conn.Close()
			return
		}

		// Check if email is in allowed list
		allowed := false
		for _, allowedEmail := range roomInfo.AllowedEmails {
			if allowedEmail == email {
				allowed = true
				break
			}
		}

		if !allowed {
			log.Printf("Access denied: Email %s not authorized for private room %s", email, roomID)
			conn, _ := upgrader.Upgrade(c.Writer, c.Request, nil)
			conn.WriteJSON(map[string]string{"error": "access_denied", "message": "You are not authorized to join this private room"})
			conn.Close()
			return
		}

		log.Printf("Access granted: Email %s authorized for private room %s", email, roomID)
	}

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}

	room := roomManager.getOrCreateRoom(roomID, roomInfo != nil && roomInfo.IsPrivate)

	client := &Client{
		conn:  conn,
		send:  make(chan []byte, 256),
		room:  room,
		email: email,
	}

	room.register <- client

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

	if err := mongoClient.Ping(ctx, nil); err != nil {
		log.Fatal("MongoDB ping error:", err)
	}
	log.Println("Connected to MongoDB!")

	roomManager = newRoomManager(mongoClient)

	r := gin.Default()
	r.LoadHTMLGlob("templates/*")
	
	r.GET("/", func(c *gin.Context) {
		c.HTML(http.StatusOK, "index.html", nil)
	})

	r.GET("/room/:roomId", func(c *gin.Context) {
		c.HTML(http.StatusOK, "room.html", gin.H{
			"roomId": c.Param("roomId"),
		})
	})

	// Create room
	r.POST("/api/room/create", func(c *gin.Context) {
		var req struct {
			RoomID       string `json:"roomId"`
			IsPrivate    bool   `json:"isPrivate"`
			CreatorEmail string `json:"creatorEmail"`
		}

		if err := c.BindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
			return
		}

		roomInfo := RoomInfo{
			RoomID:       req.RoomID,
			IsPrivate:    req.IsPrivate,
			CreatorEmail: req.CreatorEmail,
			AllowedEmails: []string{req.CreatorEmail},
			CreatedAt:    time.Now(),
		}

		if err := roomManager.saveRoomInfo(roomInfo); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create room"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true, "roomId": req.RoomID})
	})

	// Request OTP for private room
	r.POST("/api/room/request-otp", func(c *gin.Context) {
		var req struct {
			RoomID string `json:"roomId"`
			Email  string `json:"email"`
		}

		if err := c.BindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
			return
		}

		roomInfo, err := roomManager.getRoomInfo(req.RoomID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Room not found"})
			return
		}

		if !roomInfo.IsPrivate {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Room is not private"})
			return
		}

		otp := generateOTP()
		otpKey := req.RoomID + ":" + req.Email

		roomManager.otpMu.Lock()
		roomManager.otpStore[otpKey] = &OTP{
			Email:     req.Email,
			Code:      otp,
			RoomID:    req.RoomID,
			ExpiresAt: time.Now().Add(5 * time.Minute),
		}
		roomManager.otpMu.Unlock()

		// Send OTP via email
		if err := sendOTPEmail(req.Email, otp, req.RoomID); err != nil {
			log.Printf("Error sending email: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to send OTP"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true, "message": "OTP sent to email"})
	})

	// Verify OTP
	r.POST("/api/room/verify-otp", func(c *gin.Context) {
		var req struct {
			RoomID string `json:"roomId"`
			Email  string `json:"email"`
			OTP    string `json:"otp"`
		}

		if err := c.BindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
			return
		}

		otpKey := req.RoomID + ":" + req.Email

		roomManager.otpMu.RLock()
		storedOTP, exists := roomManager.otpStore[otpKey]
		roomManager.otpMu.RUnlock()

		if !exists {
			c.JSON(http.StatusBadRequest, gin.H{"error": "OTP not found or expired"})
			return
		}

		if time.Now().After(storedOTP.ExpiresAt) {
			roomManager.otpMu.Lock()
			delete(roomManager.otpStore, otpKey)
			roomManager.otpMu.Unlock()
			c.JSON(http.StatusBadRequest, gin.H{"error": "OTP expired"})
			return
		}

		if storedOTP.Code != req.OTP {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid OTP"})
			return
		}

		// Add email to allowed list
		if err := roomManager.addAllowedEmail(req.RoomID, req.Email); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify"})
			return
		}

		roomManager.otpMu.Lock()
		delete(roomManager.otpStore, otpKey)
		roomManager.otpMu.Unlock()

		c.JSON(http.StatusOK, gin.H{"success": true, "message": "OTP verified"})
	})

	// Check if email is allowed in private room
	r.GET("/api/room/:roomId/check-access/:email", func(c *gin.Context) {
		roomID := c.Param("roomId")
		email := c.Param("email")

		roomInfo, err := roomManager.getRoomInfo(roomID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Room not found"})
			return
		}

		if !roomInfo.IsPrivate {
			c.JSON(http.StatusOK, gin.H{"allowed": true, "isPrivate": false})
			return
		}

		allowed := false
		for _, allowedEmail := range roomInfo.AllowedEmails {
			if allowedEmail == email {
				allowed = true
				break
			}
		}

		c.JSON(http.StatusOK, gin.H{"allowed": allowed, "isPrivate": true})
	})

	// Get room info (new endpoint)
	r.GET("/api/room/:roomId/info", func(c *gin.Context) {
		roomID := c.Param("roomId")

		roomInfo, err := roomManager.getRoomInfo(roomID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Room not found"})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"roomId":    roomInfo.RoomID,
			"isPrivate": roomInfo.IsPrivate,
			"createdAt": roomInfo.CreatedAt,
		})
	})

	r.GET("/ws/:roomId", handleWebSocket)

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
				"isPrivate":   room.isPrivate,
			})
		}
		c.JSON(http.StatusOK, gin.H{"rooms": rooms})
	})

	log.Println("Server starting on :8080")
	if err := r.Run(":8080"); err != nil {
		log.Fatal("Server error:", err)
	}
}