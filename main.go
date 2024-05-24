package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/joho/godotenv"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// App represents a backend application with its usage count
type App struct {
	Port  int `bson:"port"`
	Count int `bson:"count"`
}

// UsageData stores usage counts of all apps
type UsageData struct {
	sync.Mutex
	Apps map[int]*App
}

var usageData = UsageData{
	Apps: make(map[int]*App),
}

func main() {

	// Load environment variables from .env file
	err := godotenv.Load()
	if err != nil {
		log.Fatalf("Error loading .env file: %v", err)
	}

	// MongoDB connection settings from environment variables
	mongoURI := os.Getenv("MONGO_URI")
	mongoDatabase := os.Getenv("MONGO_DATABASE")
	mongoCollection := os.Getenv("MONGO_COLLECTION")

	appPort := os.Getenv("APP_PORT")

	// Connect to MongoDB
	clientOpts := options.Client().ApplyURI(mongoURI)
	client, err := mongo.NewClient(clientOpts)
	if err != nil {
		log.Fatalf("Error creating MongoDB client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err = client.Connect(ctx)
	if err != nil {
		log.Fatalf("Error connecting to MongoDB: %v", err)
	}
	defer func() {
		if err := client.Disconnect(ctx); err != nil {
			log.Fatalf("Error disconnecting from MongoDB: %v", err)
		}
	}()

	// Retrieve existing counts from MongoDB
	collection := client.Database(mongoDatabase).Collection(mongoCollection)
	cursor, err := collection.Find(ctx, bson.M{})
	if err != nil {
		log.Fatalf("Error retrieving counts from MongoDB: %v", err)
	}
	defer cursor.Close(ctx)

	for cursor.Next(ctx) {
		var app App
		if err := cursor.Decode(&app); err != nil {
			log.Printf("Error decoding app from MongoDB: %v", err)
			continue
		}
		usageData.Lock()
		usageData.Apps[app.Port] = &app
		usageData.Unlock()
	}

	if err := cursor.Err(); err != nil {
		log.Fatalf("Cursor error: %v", err)
	}

	// Set up the router
	r := chi.NewRouter()
	r.Use(middleware.Logger)

	// Proxy routes to backend applications
	r.HandleFunc("/{appID}", func(w http.ResponseWriter, r *http.Request) {
		appID := chi.URLParam(r, "appID")
		port, err := strconv.Atoi(appID)
		if err != nil {
			http.Error(w, "Invalid application ID", http.StatusBadRequest)
			return
		}

		proxyRequest(port, w, r)

		// Increment usage count
		usageData.Lock()
		if app, ok := usageData.Apps[port]; ok {
			app.Count++
			// Update count in MongoDB
			filter := bson.M{"port": port}
			update := bson.M{"$set": bson.M{"count": app.Count}}

			updateCtx, updateCancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer updateCancel()
			_, err := collection.UpdateOne(updateCtx, filter, update)
			if err != nil {
				log.Printf("Error updating MongoDB count for port %d: %v", port, err)
			}
		} else {
			log.Printf("App for port %d not found", port)
		}
		usageData.Unlock()
	})

	log.Printf("Starting server on port %s", appPort)
	http.ListenAndServe(":"+appPort, r)
}

func proxyRequest(port int, w http.ResponseWriter, r *http.Request) {
	proxyURL := fmt.Sprintf("http://localhost:%d%s", port, r.URL.Path)
	req, err := http.NewRequest(r.Method, proxyURL, r.Body)
	if err != nil {
		http.Error(w, "Error creating request", http.StatusInternalServerError)
		return
	}
	req.Header = r.Header

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "Error forwarding request", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
