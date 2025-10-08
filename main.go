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

// RoomInfo struct for MongoDB
type RoomInfo struct {
	RoomID       string    `bson:"room_id" json:"roomId"`
	IsPrivate    bool      `bson:"is_private" json:"isPrivate"`
	CreatorName  string    `bson:"creator_name" json:"creatorName"`
	CreatedAt    time.Time `bson:"created_at" json:"createdAt"`
}

// JoinRequest represents a pending join request
type JoinRequest struct {
	UserName  string
	UserID    string
	Timestamp time.Time
	Response  chan bool // Channel to receive approval/rejection
}

// Client represents a WebSocket connection
type Client struct {
	conn     *websocket.Conn
	send     chan []byte
	room     *Room
	userID   string
	userName string
	isCreator bool
}

// Room represents a chat room
type Room struct {
	id           string
	clients      map[*Client]bool
	broadcast    chan []byte
	register     chan *Client
	unregister   chan *Client
	joinRequests map[string]*JoinRequest
	requestMu    sync.Mutex
	mu           sync.Mutex
	manager      *RoomManager
	isPrivate    bool
	creatorName  string
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

func (rm *RoomManager) getOrCreateRoom(roomID string, isPrivate bool, creatorName string) *Room {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	if room, exists := rm.rooms[roomID]; exists {
		return room
	}

	room := &Room{
		id:           roomID,
		clients:      make(map[*Client]bool),
		broadcast:    make(chan []byte, 256),
		register:     make(chan *Client),
		unregister:   make(chan *Client),
		joinRequests: make(map[string]*JoinRequest),
		manager:      rm,
		isPrivate:    isPrivate,
		creatorName:  creatorName,
	}

	rm.rooms[roomID] = room
	go room.run()

	log.Printf("Room created: %s (Private: %v, Creator: %s)", roomID, isPrivate, creatorName)
	return room
}

func (rm *RoomManager) deleteRoom(roomID string) {
	rm.mu.Lock()
	delete(rm.rooms, roomID)
	rm.mu.Unlock()

	log.Printf("Room deleted from memory: %s", roomID)

	// Delete room and messages from database in background
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// Delete room info
		roomCollection := rm.db.Collection("rooms")
		_, err := roomCollection.DeleteOne(ctx, bson.M{"room_id": roomID})
		if err != nil {
			log.Printf("Error deleting room info from database: %v", err)
		} else {
			log.Printf("Room info deleted from database: %s", roomID)
		}

		// Delete all messages in this room
		messagesCollection := rm.db.Collection("messages")
		result, err := messagesCollection.DeleteMany(ctx, bson.M{"room_id": roomID})
		if err != nil {
			log.Printf("Error deleting room messages from database: %v", err)
		} else {
			log.Printf("Deleted %d messages for room: %s", result.DeletedCount, roomID)
		}
	}()
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

	// Check if room already exists
	var existingRoom RoomInfo
	err := collection.FindOne(ctx, bson.M{"room_id": roomInfo.RoomID}).Decode(&existingRoom)

	if err == mongo.ErrNoDocuments {
		// Room doesn't exist, create it
		_, err = collection.InsertOne(ctx, roomInfo)
		return err
	} else if err != nil {
		// Some other error occurred
		return err
	}

	// Room already exists
	log.Printf("Room %s already exists in database", roomInfo.RoomID)
	return nil
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

func (r *Room) sendJoinRequest(userName, userID string) bool {
	// Create join request
	request := &JoinRequest{
		UserName:  userName,
		UserID:    userID,
		Timestamp: time.Now(),
		Response:  make(chan bool),
	}

	r.requestMu.Lock()
	r.joinRequests[userID] = request
	r.requestMu.Unlock()

	// Notify creator
	notification := map[string]interface{}{
		"type":     "join_request",
		"userName": userName,
		"userID":   userID,
	}
	notificationBytes, _ := json.Marshal(notification)

	r.mu.Lock()
	for client := range r.clients {
		if client.isCreator {
			client.send <- notificationBytes
		}
	}
	r.mu.Unlock()

	// Wait for response with timeout
	select {
	case approved := <-request.Response:
		r.requestMu.Lock()
		delete(r.joinRequests, userID)
		r.requestMu.Unlock()
		return approved
	case <-time.After(60 * time.Second):
		r.requestMu.Lock()
		delete(r.joinRequests, userID)
		r.requestMu.Unlock()
		return false
	}
}

func (r *Room) handleJoinResponse(userID string, approved bool) {
	r.requestMu.Lock()
	request, exists := r.joinRequests[userID]
	r.requestMu.Unlock()

	if exists {
		request.Response <- approved
	}
}

func (r *Room) getActiveUsers() []map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()

	users := make([]map[string]string, 0)
	for client := range r.clients {
		if !client.isCreator {
			users = append(users, map[string]string{
				"userID":   client.userID,
				"userName": client.userName,
			})
		}
	}
	return users
}

func (r *Room) transferCreator(newCreatorID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for client := range r.clients {
		if client.userID == newCreatorID {
			client.isCreator = true
			r.creatorName = client.userName

			// Notify all clients about new creator
			notification := map[string]interface{}{
				"type":           "creator_changed",
				"newCreatorName": client.userName,
			}
			notificationBytes, _ := json.Marshal(notification)
			
			for c := range r.clients {
				c.send <- notificationBytes
			}

			// Update database
			go func(roomID, creatorName string) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()

				collection := r.manager.db.Collection("rooms")
				_, err := collection.UpdateOne(
					ctx,
					bson.M{"room_id": roomID},
					bson.M{"$set": bson.M{"creator_name": creatorName}},
				)
				if err != nil {
					log.Printf("Error updating creator: %v", err)
				}
			}(r.id, client.userName)

			break
		}
	}
}

func (r *Room) notifyRoomDeletion() {
	r.mu.Lock()
	defer r.mu.Unlock()

	notification := map[string]interface{}{
		"type":    "room_deleted",
		"message": "Room has been deleted by the creator",
	}
	notificationBytes, _ := json.Marshal(notification)

	for client := range r.clients {
		client.send <- notificationBytes
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

		// Check if this is a join approval response
		if msgType, ok := incomingMsg["type"].(string); ok && msgType == "join_response" {
			if c.isCreator {
				userID := incomingMsg["userID"].(string)
				approved := incomingMsg["approved"].(bool)
				c.room.handleJoinResponse(userID, approved)
			}
			continue
		}

		// Check if this is a leave request
		if msgType, ok := incomingMsg["type"].(string); ok && msgType == "leave_request" {
			if c.isCreator {
				// Send active users list to creator
				users := c.room.getActiveUsers()
				response := map[string]interface{}{
					"type":  "active_users",
					"users": users,
				}
				responseBytes, _ := json.Marshal(response)
				c.send <- responseBytes
			} else {
				// Regular user leaving, just disconnect
				return
			}
			continue
		}

		// Check if this is a transfer creator request
		if msgType, ok := incomingMsg["type"].(string); ok && msgType == "transfer_creator" {
			if c.isCreator {
				newCreatorID := incomingMsg["newCreatorID"].(string)
				c.room.transferCreator(newCreatorID)
				c.isCreator = false
				// Now disconnect the old creator
				return
			}
			continue
		}

		// Check if this is a delete room request
		if msgType, ok := incomingMsg["type"].(string); ok && msgType == "delete_room" {
			if c.isCreator {
				c.room.notifyRoomDeletion()
				// Small delay to ensure notification is sent
				time.Sleep(500 * time.Millisecond)
				// Disconnect all clients
				return
			}
			continue
		}

		// Regular chat message
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
	userName := c.Query("userName")
	userID := c.Query("userID")
	isCreator := c.Query("isCreator") == "true"

	if roomID == "" || userName == "" || userID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing parameters"})
		return
	}

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}

	// Get room info
	roomInfo, err := roomManager.getRoomInfo(roomID)
	var room *Room

	if err == nil {
		// Room exists
		room = roomManager.getOrCreateRoom(roomID, roomInfo.IsPrivate, roomInfo.CreatorName)

		// If it's a private room and user is not the creator, request approval
		if roomInfo.IsPrivate && !isCreator {
			// Send waiting message
			conn.WriteJSON(map[string]string{"type": "waiting_approval", "message": "Waiting for room creator approval..."})

			// Request approval
			approved := room.sendJoinRequest(userName, userID)

			if !approved {
				conn.WriteJSON(map[string]string{"type": "access_denied", "message": "Join request denied by room creator"})
				conn.Close()
				return
			}

			// Approved, send success
			conn.WriteJSON(map[string]string{"type": "approved", "message": "Join request approved!"})
		}
	} else {
		// Room doesn't exist, this user is creating it
		room = roomManager.getOrCreateRoom(roomID, false, userName)
	}

	client := &Client{
		conn:      conn,
		send:      make(chan []byte, 256),
		room:      room,
		userID:    userID,
		userName:  userName,
		isCreator: isCreator || room.creatorName == userName,
	}

	room.register <- client

	// Send message history
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
			RoomID      string `json:"roomId"`
			IsPrivate   bool   `json:"isPrivate"`
			CreatorName string `json:"creatorName"`
		}

		if err := c.BindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
			return
		}

		roomInfo := RoomInfo{
			RoomID:      req.RoomID,
			IsPrivate:   req.IsPrivate,
			CreatorName: req.CreatorName,
			CreatedAt:   time.Now(),
		}

		if err := roomManager.saveRoomInfo(roomInfo); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create room"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true, "roomId": req.RoomID})
	})

	// Get room info
	r.GET("/api/room/:roomId/info", func(c *gin.Context) {
		roomID := c.Param("roomId")

		roomInfo, err := roomManager.getRoomInfo(roomID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Room not found"})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"roomId":      roomInfo.RoomID,
			"isPrivate":   roomInfo.IsPrivate,
			"creatorName": roomInfo.CreatorName,
			"createdAt":   roomInfo.CreatedAt,
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