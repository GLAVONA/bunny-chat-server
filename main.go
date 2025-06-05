package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/go-chi/chi/middleware"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"
	"github.com/joho/godotenv"
	_ "modernc.org/sqlite"
)

// Global variable for the database connection
var db *sql.DB

const DB_FILE_NAME = "chat_messages.db" // Define the database file name

func main() {
	log.Println("Starting chat server...")

	// Load environment variables from .env file
	loadEnvFile()

	// Initialize database
	var err error
	db, err = initDb()
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}

	defer func() {
		if err := db.Close(); err != nil {
			log.Printf("Error closing database connection: %v", err)
		} else {
			log.Println("Database connection closed.")
		}
	}()

	// Create the messages table
	createTable(db, "messages", `(
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    type TEXT NOT NULL,
    username TEXT NOT NULL,
    content TEXT,
    room TEXT NOT NULL,
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    image_data TEXT,
    image_type TEXT
);`)

	// Drop and recreate the reactions table
	_, err = db.Exec("DROP TABLE IF EXISTS reactions")
	if err != nil {
		log.Printf("Error dropping reactions table: %v", err)
	}

	// Create the reactions table
	createTable(db, "reactions", `(
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    message_id INTEGER NOT NULL,
    username TEXT NOT NULL,
    reaction TEXT NOT NULL,
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (message_id) REFERENCES messages(id) ON DELETE CASCADE,
    UNIQUE(message_id, username, reaction)
);`)

	// Add image columns if they don't exist
	addImageColumns(db)

	// Create the sessions table
	createTable(db, "sessions", `(
    id TEXT PRIMARY KEY,
    username TEXT NOT NULL,
    room TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    last_accessed DATETIME DEFAULT CURRENT_TIMESTAMP,
    expires_at DATETIME NOT NULL
);`)

	// Initialize authentication service
	authService, err := NewAuthService()
	if err != nil {
		log.Fatalf("Failed to initialize authentication service: %v", err)
	}

	// Initialize session service
	sessionService := NewSessionService(db)

	// Create hub and start it
	hub := NewHub(db)
	go hub.Run() // Start the hub's goroutine

	// Setup router
	r := chi.NewRouter()

	// CORS middleware configuration
	corsMiddleware := cors.New(cors.Options{
		// Allow requests from both development and production origins
		AllowedOrigins: []string{
			"http://localhost:5173", // Vite dev server
			"http://127.0.0.1:5173", // Vite dev server alternative
			"http://localhost:3000", // Alternative dev port
			"http://127.0.0.1:3000", // Alternative dev port
			"https://lulunajiji.me",
		},
		// Allow these HTTP methods
		AllowedMethods: []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		// Allow these headers
		AllowedHeaders: []string{
			"Accept",
			"Authorization",
			"Content-Type",
			"X-CSRF-Token",
			"Origin",
			"X-Requested-With",
		},
		// Allow credentials (cookies, authorization headers, etc)
		AllowCredentials: true,
		// Cache preflight requests for 5 minutes
		MaxAge: 300,
		// Expose headers to the client
		ExposedHeaders: []string{
			"Set-Cookie",
			"Access-Control-Allow-Credentials",
		},
	})

	// Apply middleware
	r.Use(corsMiddleware.Handler)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// Authentication endpoint
	r.Post("/auth", func(w http.ResponseWriter, r *http.Request) {
		HandleAuth(authService, sessionService, w, r)
	})

	// Session validation endpoint
	r.Get("/session", func(w http.ResponseWriter, r *http.Request) {
		HandleSessionCheck(sessionService, w, r)
	})

	// Logout endpoint
	r.Post("/logout", func(w http.ResponseWriter, r *http.Request) {
		HandleLogout(sessionService, w, r)
	})

	// WebSocket endpoint
	r.Get("/ws", func(w http.ResponseWriter, r *http.Request) {
		ServeWs(hub, authService, sessionService, w, r)
	})

	// GIPHY search endpoint
	r.Get("/giphy/search", func(w http.ResponseWriter, r *http.Request) {
		HandleGiphySearch(w, r, sessionService)
	})

	// Health check endpoint
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok","service":"chat-server"}`))
	})

	// Print configuration info
	printStartupInfo()

	// Validate GIPHY API key
	if os.Getenv("GIPHY_API_KEY") == "" {
		log.Fatal("GIPHY_API_KEY environment variable is not set")
	}

	// Get server port from environment or default to 8080
	port := getEnvOrDefault("PORT", "8080")
	addr := ":" + port

	log.Printf("Server starting on %s", addr)
	log.Println("Authentication endpoint: POST /auth")
	log.Println("Session check endpoint: GET /session")
	log.Println("Logout endpoint: POST /logout")
	log.Println("WebSocket endpoint: /ws")
	log.Println("GIPHY search endpoint: GET /giphy/search")
	log.Println("Health check endpoint: /health")

	if err := http.ListenAndServe(addr, r); err != nil {
		log.Fatalf("ListenAndServe error: %v", err)
	}
}

// initDb initializes the SQLite database connection.
func initDb() (*sql.DB, error) {
	// Get database file path from environment or use default
	dbFile := getEnvOrDefault("DB_FILE", DB_FILE_NAME)

	dbConn, err := sql.Open("sqlite", dbFile)
	if err != nil {
		return nil, fmt.Errorf("error opening database: %w", err)
	}

	// Configure connection pool
	dbConn.SetMaxOpenConns(25)
	dbConn.SetMaxIdleConns(5)

	err = dbConn.Ping()
	if err != nil {
		dbConn.Close()
		return nil, fmt.Errorf("error connecting to the database: %w", err)
	}

	log.Printf("Database '%s' connected successfully.", dbFile)
	return dbConn, nil
}

// createTable creates a table if it doesn't exist
func createTable(db *sql.DB, name string, schema string) {
	createTableSQL := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s %s`, name, schema)
	_, err := db.Exec(createTableSQL)
	if err != nil {
		log.Fatalf("Couldn't create table '%s': %v", name, err)
	}
	log.Printf("Table '%s' ensured in database.", name)
}

// loadEnvFile loads environment variables from .env file if it exists
func loadEnvFile() {
	// Try to load .env file, but don't fail if it doesn't exist
	envFile := getEnvOrDefault("ENV_FILE", ".env")

	if err := godotenv.Load(envFile); err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Warning: Error loading %s file: %v", envFile, err)
		} else {
			log.Printf("No %s file found, using system environment variables", envFile)
		}
	} else {
		log.Printf("Loaded environment variables from %s", envFile)
	}
}

// getEnvOrDefault returns environment variable value or default if not set
func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// printStartupInfo logs configuration information at startup
func printStartupInfo() {
	log.Println("=== Chat Server Configuration ===")
	log.Printf("Database file: %s", getEnvOrDefault("DB_FILE", DB_FILE_NAME))
	log.Printf("Server port: %s", getEnvOrDefault("PORT", "8080"))

	// Log auth configuration (without revealing tokens)
	if os.Getenv("CHAT_AUTH_TOKENS") != "" {
		log.Println("Auth: Using CHAT_AUTH_TOKENS")
	} else if os.Getenv("CHAT_AUTH_TOKEN") != "" {
		log.Println("Auth: Using CHAT_AUTH_TOKEN")
	} else if os.Getenv("CHAT_TOKEN_FILE") != "" {
		log.Printf("Auth: Using token file: %s", os.Getenv("CHAT_TOKEN_FILE"))
	}

	// Log environment info
	if os.Getenv("GO_ENV") != "" {
		log.Printf("Environment: %s", os.Getenv("GO_ENV"))
	}
	log.Println("=================================")
}

// addImageColumns adds image-related columns to the messages table if they don't exist
func addImageColumns(db *sql.DB) {
	// Check if image_data column exists
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name='image_data'").Scan(&count)
	if err != nil {
		log.Printf("Error checking for image_data column: %v", err)
		return
	}

	// Add columns if they don't exist
	if count == 0 {
		alterSQL := `ALTER TABLE messages ADD COLUMN image_data TEXT;
                    ALTER TABLE messages ADD COLUMN image_type TEXT;`
		_, err := db.Exec(alterSQL)
		if err != nil {
			log.Printf("Error adding image columns: %v", err)
		} else {
			log.Println("Added image columns to messages table")
		}
	}
}
