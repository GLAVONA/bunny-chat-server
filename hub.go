package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	DefaultRoom       = "DEFAULT_ROOM"
	WriteWait         = 10 * time.Second
	PongWait          = 60 * time.Second
	PingPeriod        = (PongWait * 9) / 10
	MaxMessageSize    = 512
	SessionDuration   = 1 * time.Hour // Changed from 7 days to 1 hour
	SessionCookieName = "chat_session"
)

// MessageType represents the different types of messages
type MessageType string

const (
	ChatMessage  MessageType = "chat"
	JoinMessage  MessageType = "join"
	LeaveMessage MessageType = "leave"
	HistoryBatch MessageType = "history_batch"
	ErrorMessage MessageType = "error"
)

// Message represents a message in the chat system
type Message struct {
	Type      MessageType `json:"type"`
	Username  string      `json:"username"`
	Content   string      `json:"content,omitempty"`
	Room      string      `json:"room,omitempty"`
	UserList  []string    `json:"userList,omitempty"`
	Timestamp string      `json:"timestamp,omitempty"`
	History   []Message   `json:"history,omitempty"`
}

// Client represents a connected WebSocket client
type Client struct {
	conn     *websocket.Conn
	send     chan Message
	hub      *Hub
	username string
	room     string
}

// Hub manages all client connections and message broadcasting
type Hub struct {
	rooms      map[string]map[*Client]bool
	broadcast  chan Message
	register   chan *Client
	unregister chan *Client
	mu         sync.RWMutex
	db         *sql.DB
}

// Session represents a user session
type Session struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	Room         string    `json:"room"`
	CreatedAt    time.Time `json:"created_at"`
	LastAccessed time.Time `json:"last_accessed"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// SessionService manages user sessions
type SessionService struct {
	db *sql.DB
	mu sync.RWMutex
}

// AuthRequest represents the authentication request
type AuthRequest struct {
	Token    string `json:"token"`
	Username string `json:"username"`
	Room     string `json:"room,omitempty"`
}

// AuthResponse represents the authentication response
type AuthResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message,omitempty"`
	Username string `json:"username,omitempty"`
	Room     string `json:"room,omitempty"`
}

// SessionResponse represents the session check response
type SessionResponse struct {
	Valid    bool   `json:"valid"`
	Username string `json:"username,omitempty"`
	Room     string `json:"room,omitempty"`
}

// NewSessionService creates a new session service
func NewSessionService(database *sql.DB) *SessionService {
	service := &SessionService{
		db: database,
	}

	// Clean up expired sessions on startup
	go service.cleanupExpiredSessions()

	return service
}

// CreateSession creates a new session
func (s *SessionService) CreateSession(username, room string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sessionID, err := generateSessionID()
	if err != nil {
		return nil, fmt.Errorf("failed to generate session ID: %w", err)
	}

	now := time.Now()
	expiresAt := now.Add(SessionDuration)

	session := &Session{
		ID:           sessionID,
		Username:     username,
		Room:         room,
		CreatedAt:    now,
		LastAccessed: now,
		ExpiresAt:    expiresAt,
	}

	query := `INSERT INTO sessions (id, username, room, created_at, last_accessed, expires_at) 
			  VALUES (?, ?, ?, ?, ?, ?)`

	_, err = s.db.Exec(query, session.ID, session.Username, session.Room,
		session.CreatedAt.Format(time.RFC3339),
		session.LastAccessed.Format(time.RFC3339),
		session.ExpiresAt.Format(time.RFC3339))

	if err != nil {
		return nil, fmt.Errorf("failed to save session: %w", err)
	}

	log.Printf("Created session for user %s in room %s (expires: %s)",
		username, room, expiresAt.Format(time.RFC3339))

	return session, nil
}

// GetSession retrieves a session by ID
func (s *SessionService) GetSession(sessionID string) (*Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `SELECT id, username, room, created_at, last_accessed, expires_at 
			  FROM sessions WHERE id = ? AND expires_at > datetime('now')`

	row := s.db.QueryRow(query, sessionID)

	var session Session
	var createdAtStr, lastAccessedStr, expiresAtStr string

	err := row.Scan(&session.ID, &session.Username, &session.Room,
		&createdAtStr, &lastAccessedStr, &expiresAtStr)

	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("session not found or expired")
		}
		return nil, fmt.Errorf("failed to retrieve session: %w", err)
	}

	// Parse timestamps
	if session.CreatedAt, err = time.Parse(time.RFC3339, createdAtStr); err != nil {
		return nil, fmt.Errorf("failed to parse created_at: %w", err)
	}
	if session.LastAccessed, err = time.Parse(time.RFC3339, lastAccessedStr); err != nil {
		return nil, fmt.Errorf("failed to parse last_accessed: %w", err)
	}
	if session.ExpiresAt, err = time.Parse(time.RFC3339, expiresAtStr); err != nil {
		return nil, fmt.Errorf("failed to parse expires_at: %w", err)
	}

	return &session, nil
}

// UpdateSessionAccess updates the last accessed time for a session
func (s *SessionService) UpdateSessionAccess(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	expiresAt := now.Add(SessionDuration)

	query := `UPDATE sessions 
			  SET last_accessed = ?, expires_at = ? 
			  WHERE id = ? AND expires_at > datetime('now')`

	result, err := s.db.Exec(query,
		now.Format(time.RFC3339),
		expiresAt.Format(time.RFC3339),
		sessionID)

	if err != nil {
		return fmt.Errorf("failed to update session access: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return fmt.Errorf("session not found or expired")
	}

	return nil
}

// DeleteSession removes a session
func (s *SessionService) DeleteSession(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `DELETE FROM sessions WHERE id = ?`
	_, err := s.db.Exec(query, sessionID)

	if err == nil {
		log.Printf("Deleted session: %s", sessionID)
	}

	return err
}

// cleanupExpiredSessions periodically cleans up expired sessions
func (s *SessionService) cleanupExpiredSessions() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		s.mu.Lock()
		query := `DELETE FROM sessions WHERE expires_at <= datetime('now')`
		result, err := s.db.Exec(query)
		if err != nil {
			log.Printf("Error cleaning up expired sessions: %v", err)
		} else {
			rowsAffected, _ := result.RowsAffected()
			if rowsAffected > 0 {
				log.Printf("Cleaned up %d expired sessions", rowsAffected)
			}
		}
		s.mu.Unlock()
	}
}

// generateSessionID generates a secure random session ID
func generateSessionID() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// HandleAuth handles authentication and session creation
func HandleAuth(authService *AuthService, sessionService *SessionService, w http.ResponseWriter, r *http.Request) {
	var authReq AuthRequest
	if err := json.NewDecoder(r.Body).Decode(&authReq); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Validate token
	if !authService.IsValidToken(authReq.Token) {
		log.Printf("Invalid token attempt for user %s", authReq.Username)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(AuthResponse{
			Success: false,
			Message: "Invalid authentication token",
		})
		return
	}

	// Validate username
	if authReq.Username == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(AuthResponse{
			Success: false,
			Message: "Username is required",
		})
		return
	}

	// Set default room if not provided
	if authReq.Room == "" {
		authReq.Room = DefaultRoom
	}

	// Create session
	session, err := sessionService.CreateSession(authReq.Username, authReq.Room)
	if err != nil {
		log.Printf("Failed to create session for %s: %v", authReq.Username, err)
		http.Error(w, "Failed to create session", http.StatusInternalServerError)
		return
	}

	// Set CORS headers first
	w.Header().Set("Access-Control-Allow-Credentials", "true")
	w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Content-Type", "application/json")

	// Get domain from host
	domain := r.Host
	if idx := strings.Index(domain, ":"); idx != -1 {
		domain = domain[:idx]
	}

	// Log cookie settings for debugging
	log.Printf("Setting cookie with domain: %s, secure: %v, samesite: %v",
		domain, true, http.SameSiteNoneMode)

	cookie := &http.Cookie{
		Name:     SessionCookieName,
		Value:    session.ID,
		Path:     "/",
		Domain:   domain,
		HttpOnly: true,
		Secure:   true,                  // Required for SameSite=None
		SameSite: http.SameSiteNoneMode, // Allow cross-origin requests
		Expires:  session.ExpiresAt,
		MaxAge:   int(SessionDuration.Seconds()),
	}
	http.SetCookie(w, cookie)

	// Log response headers for debugging
	log.Printf("Response headers after setting cookie: %v", w.Header())

	// Send response
	json.NewEncoder(w).Encode(AuthResponse{
		Success:  true,
		Username: session.Username,
		Room:     session.Room,
	})

	log.Printf("User %s authenticated successfully for room %s", authReq.Username, authReq.Room)
}

// HandleSessionCheck validates an existing session
func HandleSessionCheck(sessionService *SessionService, w http.ResponseWriter, r *http.Request) {
	// Set CORS headers
	w.Header().Set("Access-Control-Allow-Credentials", "true")
	w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Content-Type", "application/json")

	cookie, err := r.Cookie(SessionCookieName)
	fmt.Println("Cookie:", cookie)
	if err != nil {
		json.NewEncoder(w).Encode(SessionResponse{Valid: false})
		return
	}

	session, err := sessionService.GetSession(cookie.Value)
	if err != nil {
		json.NewEncoder(w).Encode(SessionResponse{Valid: false})
		return
	}

	// Update last accessed time
	sessionService.UpdateSessionAccess(session.ID)

	json.NewEncoder(w).Encode(SessionResponse{
		Valid:    true,
		Username: session.Username,
		Room:     session.Room,
	})
}

// HandleLogout handles user logout
func HandleLogout(sessionService *SessionService, w http.ResponseWriter, r *http.Request) {
	// Set CORS headers
	w.Header().Set("Access-Control-Allow-Credentials", "true")
	w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Content-Type", "application/json")

	cookie, err := r.Cookie(SessionCookieName)
	if err == nil {
		sessionService.DeleteSession(cookie.Value)
	}

	// Clear the cookie
	domain := r.Host
	if idx := strings.Index(domain, ":"); idx != -1 {
		domain = domain[:idx]
	}

	// Set CORS headers first
	w.Header().Set("Access-Control-Allow-Credentials", "true")
	w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Content-Type", "application/json")

	// Clear the session cookie
	clearCookie := http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		Domain:   domain,
		HttpOnly: true,
		Secure:   true,                  // Required for SameSite=None
		SameSite: http.SameSiteNoneMode, // Allow cross-origin requests
		Expires:  time.Unix(0, 0),
	}
	http.SetCookie(w, &clearCookie)

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// NewHub creates a new Hub instance
func NewHub(database *sql.DB) *Hub {
	return &Hub{
		rooms:      make(map[string]map[*Client]bool),
		broadcast:  make(chan Message, 256),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		db:         database,
	}
}

// Run starts the hub's main event loop
func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.handleClientRegistration(client)
		case client := <-h.unregister:
			h.handleClientUnregistration(client)
		case message := <-h.broadcast:
			h.handleBroadcast(message)
		}
	}
}

// handleClientRegistration processes new client registrations
func (h *Hub) handleClientRegistration(client *Client) {
	h.mu.Lock()

	if client.room == "" {
		client.room = DefaultRoom
	}

	// Initialize room if it doesn't exist
	if _, exists := h.rooms[client.room]; !exists {
		h.rooms[client.room] = make(map[*Client]bool)
	}

	h.rooms[client.room][client] = true
	userList := h.getUserListForRoom(client.room)

	h.mu.Unlock()

	log.Printf("Client %s registered to room %s. Total clients: %d",
		client.username, client.room, len(h.rooms[client.room]))

	// Send historical messages
	h.sendHistoricalMessages(client)

	// Notify room about new user
	joinMsg := Message{
		Type:     JoinMessage,
		Username: client.username,
		Room:     client.room,
		UserList: userList,
	}
	h.broadcastToRoom(joinMsg)
}

// handleClientUnregistration processes client disconnections
func (h *Hub) handleClientUnregistration(client *Client) {
	h.mu.Lock()

	roomClients, roomExists := h.rooms[client.room]
	if !roomExists {
		h.mu.Unlock()
		return
	}

	if _, clientExists := roomClients[client]; !clientExists {
		h.mu.Unlock()
		return
	}

	delete(roomClients, client)
	close(client.send)

	// Clean up empty room
	if len(roomClients) == 0 {
		delete(h.rooms, client.room)
		log.Printf("Room %s deleted (empty)", client.room)
		h.mu.Unlock()
		return
	}

	userList := h.getUserListForRoom(client.room)
	h.mu.Unlock()

	log.Printf("Client %s unregistered from room %s. Remaining: %d",
		client.username, client.room, len(roomClients))

	// Notify room about user leaving
	leaveMsg := Message{
		Type:     LeaveMessage,
		Username: client.username,
		Room:     client.room,
		UserList: userList,
	}
	h.broadcastToRoom(leaveMsg)
}

// handleBroadcast processes messages to be broadcast
func (h *Hub) handleBroadcast(message Message) {
	log.Printf("Broadcasting message from %s in room %s: %s",
		message.Username, message.Room, message.Content)
	h.broadcastToRoom(message)
}

// getUserListForRoom returns a list of usernames in a room (caller must hold lock)
func (h *Hub) getUserListForRoom(room string) []string {
	var userList []string
	if roomClients, exists := h.rooms[room]; exists {
		for client := range roomClients {
			userList = append(userList, client.username)
		}
	}
	return userList
}

// sendHistoricalMessages sends chat history to a newly connected client
func (h *Hub) sendHistoricalMessages(client *Client) {
	messages, err := h.getHistoricalMessages(client.room)
	if err != nil {
		log.Printf("Error fetching history for %s in room %s: %v",
			client.username, client.room, err)
		return
	}

	if len(messages) == 0 {
		log.Printf("No historical messages for %s in room %s",
			client.username, client.room)
		return
	}

	historyMsg := Message{
		Type:    HistoryBatch,
		Room:    client.room,
		History: messages,
	}

	select {
	case client.send <- historyMsg:
		log.Printf("Sent %d historical messages to %s in room %s",
			len(messages), client.username, client.room)
	default:
		log.Printf("Failed to send history to %s: channel full", client.username)
		h.unregister <- client
	}
}

// broadcastToRoom sends a message to all clients in a specific room
func (h *Hub) broadcastToRoom(message Message) {
	h.mu.RLock()
	roomClients, exists := h.rooms[message.Room]
	if !exists {
		h.mu.RUnlock()
		log.Printf("Room %s not found for broadcasting", message.Room)
		return
	}

	// Create a slice of clients to avoid holding the lock during sends
	clients := make([]*Client, 0, len(roomClients))
	for client := range roomClients {
		clients = append(clients, client)
	}
	h.mu.RUnlock()

	for _, client := range clients {
		select {
		case client.send <- message:
		default:
			log.Printf("Failed to send to %s: channel full, unregistering",
				client.username)
			h.unregister <- client
		}
	}
}

// getHistoricalMessages fetches chat history for a room from database
func (h *Hub) getHistoricalMessages(room string) ([]Message, error) {
	if h.db == nil {
		return nil, fmt.Errorf("database not available")
	}

	query := `SELECT type, username, content, room, timestamp 
			  FROM messages 
			  WHERE room = ? 
			  ORDER BY timestamp ASC`

	rows, err := h.db.Query(query, room)
	if err != nil {
		return nil, fmt.Errorf("query failed: %w", err)
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var msg Message
		var timestampStr string

		err := rows.Scan(&msg.Type, &msg.Username, &msg.Content, &msg.Room, &timestampStr)
		if err != nil {
			log.Printf("Error scanning message row: %v", err)
			continue
		}

		msg.Type = ChatMessage // Ensure history items are chat type
		msg.Timestamp = timestampStr
		messages = append(messages, msg)
	}

	return messages, rows.Err()
}

// saveMessage saves a chat message to the database
func (h *Hub) saveMessage(msg Message) error {
	if h.db == nil {
		return fmt.Errorf("database not available")
	}

	query := `INSERT INTO messages(type, username, content, room, timestamp) 
			  VALUES(?, ?, ?, ?, ?)`

	_, err := h.db.Exec(query, string(msg.Type), msg.Username, msg.Content,
		msg.Room, time.Now().Format(time.RFC3339))

	return err
}

// readPump handles reading messages from WebSocket connection
func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
		log.Printf("Client %s readPump closed", c.username)
	}()

	c.conn.SetReadLimit(MaxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(PongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(PongWait))
		return nil
	})

	for {
		var msg Message
		if err := c.conn.ReadJSON(&msg); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("Client %s unexpected close: %v", c.username, err)
			}
			break
		}

		// Set message metadata
		msg.Username = c.username
		msg.Room = c.room
		msg.Type = ChatMessage
		msg.Timestamp = time.Now().Format(time.RFC3339)

		// Save to database
		if err := c.hub.saveMessage(msg); err != nil {
			log.Printf("Error saving message from %s: %v", c.username, err)
		}

		// Broadcast to room
		c.hub.broadcast <- msg
	}
}

// writePump handles writing messages to WebSocket connection
func (c *Client) writePump() {
	ticker := time.NewTicker(PingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
		log.Printf("Client %s writePump closed", c.username)
	}()

	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(WriteWait))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			if err := c.conn.WriteJSON(message); err != nil {
				log.Printf("Client %s write error: %v", c.username, err)
				return
			}

			c.logMessageSent(message)

		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(WriteWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// logMessageSent logs sent messages with appropriate detail
func (c *Client) logMessageSent(message Message) {
	switch message.Type {
	case HistoryBatch:
		log.Printf("Sent history batch (%d messages) to %s",
			len(message.History), c.username)
	default:
		log.Printf("Sent %s message to %s: %s",
			message.Type, c.username, message.Content)
	}
}

// WebSocket upgrader configuration
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		// Allow both development and production origins
		return strings.HasPrefix(origin, "http://localhost:") ||
			strings.HasPrefix(origin, "http://127.0.0.1:") ||
			strings.HasPrefix(origin, "https://lulunajiji.me") ||
			origin == ""
	},
	EnableCompression: true,
}

// AuthService handles authentication
type AuthService struct {
	allowedTokens map[string]bool
	mu            sync.RWMutex
}

// NewAuthService creates a new authentication service
// Loads tokens from environment variables and/or token file
func NewAuthService() (*AuthService, error) {
	service := &AuthService{
		allowedTokens: make(map[string]bool),
	}

	// Load tokens from environment variable (comma-separated)
	if envTokens := os.Getenv("CHAT_AUTH_TOKENS"); envTokens != "" {
		tokens := strings.Split(envTokens, ",")
		for _, token := range tokens {
			token = strings.TrimSpace(token)
			if token != "" {
				service.allowedTokens[token] = true
				log.Printf("Loaded token from environment (length: %d)", len(token))
			}
		}
	}

	// Load tokens from file if specified
	if tokenFile := os.Getenv("CHAT_TOKEN_FILE"); tokenFile != "" {
		if err := service.loadTokensFromFile(tokenFile); err != nil {
			log.Printf("Warning: Could not load tokens from file %s: %v", tokenFile, err)
		}
	}

	// Fallback: Load from single token environment variable
	if singleToken := os.Getenv("CHAT_AUTH_TOKEN"); singleToken != "" {
		service.allowedTokens[singleToken] = true
		log.Printf("Loaded single token from CHAT_AUTH_TOKEN (length: %d)", len(singleToken))
	}

	// If no tokens loaded, return error
	if len(service.allowedTokens) == 0 {
		return nil, fmt.Errorf("no authentication tokens configured. Set CHAT_AUTH_TOKENS, CHAT_AUTH_TOKEN, or CHAT_TOKEN_FILE environment variable")
	}

	log.Printf("Authentication service initialized with %d tokens", len(service.allowedTokens))
	return service, nil
}

// loadTokensFromFile loads tokens from a file (one per line)
func (a *AuthService) loadTokensFromFile(filename string) error {
	content, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("failed to read token file: %w", err)
	}

	lines := strings.Split(string(content), "\n")
	tokensLoaded := 0

	a.mu.Lock()
	defer a.mu.Unlock()

	for _, line := range lines {
		token := strings.TrimSpace(line)
		// Skip empty lines and comments
		if token != "" && !strings.HasPrefix(token, "#") {
			a.allowedTokens[token] = true
			tokensLoaded++
		}
	}

	log.Printf("Loaded %d tokens from file %s", tokensLoaded, filename)
	return nil
}

// IsValidToken checks if a token is valid
func (a *AuthService) IsValidToken(token string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.allowedTokens[token]
}

// AddToken adds a new valid token
func (a *AuthService) AddToken(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.allowedTokens[token] = true
}

// RemoveToken removes a token
func (a *AuthService) RemoveToken(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.allowedTokens, token)
}

// ServeWs handles WebSocket upgrade requests with session support
func ServeWs(hub *Hub, auth *AuthService, sessions *SessionService, w http.ResponseWriter, r *http.Request) {
	// Try to get session from cookie first
	var username, room string
	var existingSession *Session

	// Log all cookies for debugging
	log.Printf("All cookies in request: %v", r.Cookies())

	cookie, err := r.Cookie(SessionCookieName)
	if err != nil {
		log.Printf("Cookie error: %v", err)
	} else {
		log.Printf("Found session cookie: %v", cookie)
		// Validate session
		session, err := sessions.GetSession(cookie.Value)
		if err != nil {
			log.Printf("Session validation error: %v", err)
		} else {
			username = session.Username
			room = session.Room
			existingSession = session
			// Update session access time
			sessions.UpdateSessionAccess(session.ID)
			log.Printf("WebSocket connection using session for user %s in room %s", username, room)
		}
	}

	// If no valid session, fall back to query parameters and token validation
	if username == "" {
		username = r.URL.Query().Get("username")
		room = r.URL.Query().Get("room")

		// Extract token from subprotocols
		var token string
		for _, subprotocol := range websocket.Subprotocols(r) {
			token = subprotocol
			break
		}

		// Validate input
		if username == "" {
			log.Printf("Username missing in WebSocket request")
			http.Error(w, "Username is required", http.StatusBadRequest)
			return
		}

		if token == "" {
			log.Printf("Token missing in WebSocket request")
			http.Error(w, "Authentication token required", http.StatusUnauthorized)
			return
		}

		if !auth.IsValidToken(token) {
			log.Printf("Invalid token attempt for user %s (token length: %d), token: %s", username, len(token), token)
			http.Error(w, "Invalid authentication token", http.StatusUnauthorized)
			return
		}

		// Set up subprotocols for upgrade
		upgrader.Subprotocols = []string{token}
	}

	if room == "" {
		room = DefaultRoom
	}

	// Check for duplicate username in room, but allow same user to reconnect
	if hub.isUsernameInRoom(username, room) {
		// Allow reconnection if this is the same user
		if existingSession != nil && existingSession.Username == username && existingSession.Room == room {
			log.Printf("User %s reconnecting to room %s", username, room)
		} else {
			log.Printf("Username '%s' already exists in room '%s'", username, room)
			http.Error(w, "Username already exists in this room", http.StatusBadRequest)
			return
		}
	}

	// Upgrade connection
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Failed to upgrade connection for %s: %v", username, err)
		return
	}

	log.Printf("Client connected: %s, Room: %s, RemoteAddr: %s",
		username, room, conn.RemoteAddr())

	// Create and register client
	client := &Client{
		conn:     conn,
		send:     make(chan Message, 256),
		hub:      hub,
		username: username,
		room:     room,
	}

	client.hub.register <- client

	// Start client goroutines
	go client.writePump()
	go client.readPump()
}

// isUsernameInRoom checks if a username already exists in a room
func (h *Hub) isUsernameInRoom(username, room string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if roomClients, exists := h.rooms[room]; exists {
		for client := range roomClients {
			if client.username == username {
				return true
			}
		}
	}
	return false
}
