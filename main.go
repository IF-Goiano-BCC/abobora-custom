package main

import (
	"context"
	"fmt"
	"html/template"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"time"

	"encoding/json"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})

const (
	addr = ":8080"
)

var interval = 10 // seconds

const expiry = time.Duration(24) * time.Hour

var tmpl *template.Template

var currentCode string
var clients = make(map[*websocket.Conn]bool)

type Session struct {
	Type  string // "normal" or "adm"\
	votes int
}

var sessions = make(map[string]Session) // sessionID -> Session
var sessionsMu sync.RWMutex

var allowNewSessions bool = true
var allowVotes bool = true
var max_votes_per_session int = 10
var dsn string
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

var client *s3.Client

var MINIO_BUCKET string
var MINIO_URL string

func getEnv(key, defaultVal string) string {
	val := os.Getenv(key)
	if val != "" {
		return val
	}
	return defaultVal
}

type Cosplay struct {
	ID        uint `gorm:"primaryKey"`
	Nome      string
	Desc      string
	Email     *string
	Numero    *string // optinal
	ImagePath string
}

type CosplayVote struct {
	CosplayID uint   `json:"id"`
	Desc      string `json:"name"`
	ImagePath string `json:"img"`
}
type Vote struct {
	ID               uint `gorm:"primaryKey"`
	SessionID        string
	CosplayOption1Id uint
	CosplayOption2Id uint
	CosplayVotedId   uint
	Timestamp        time.Time
}

// var err error

type CosplayWithVotes struct {
	Cosplay
	VoteCount int64
}

var db *gorm.DB

func main() {
	log.Printf("Main: Starting cosplay voting application")

	// Parse all templatesRat startup
	MINIO_BUCKET = getEnv("MINIO_BUCKET", "cosplay")
	MINIO_URL = getEnv("MINIO_URL", "localhost:9000")
	log.Printf("Main: Using MinIO bucket: %s, URL: %s", MINIO_BUCKET, MINIO_URL)

	var err error
	// var bucketName = MINIO_BUCKET
	var accountId = getEnv("R2_ACCOUNT_ID", "Miniouser")
	var accessKeyId = getEnv("R2_ACCESS_KEY_ID", "Miniouser")
	var accessKeySecret = getEnv("R2_SECRET_ACCESS_KEY", "Miniopassword")

	log.Printf("Main: Configuring AWS S3 client with account ID: %s", accountId)

	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKeyId, accessKeySecret, "")),
		config.WithRegion("auto"),
	)

	storageBackend := getEnv("STORAGE_BACKEND", "minio")
	client = s3.NewFromConfig(cfg, func(o *s3.Options) {
		if storageBackend == "r2" {
			o.BaseEndpoint = aws.String(fmt.Sprintf("https://%s.r2.cloudflarestorage.com", accountId))
		} else {
			o.BaseEndpoint = aws.String(fmt.Sprintf("http://%s", MINIO_URL))
			o.UsePathStyle = true
		}
	})

	if err != nil {
		log.Fatalln("Error initializing MinIO client:", err)
	}

	log.Printf("Main: S3 client initialized successfully")

	dsn = fmt.Sprintf("host=%s user=%s password=%s dbname=%s port=%s sslmode=disable TimeZone=UTC",
		getEnv("DB_HOST", "localhost"),
		getEnv("DB_USER", "myuser"),
		getEnv("DB_PASSWORD", " mypassword"),
		getEnv("DB_NAME", "mydb"),
		getEnv("DB_PORT", "5432"),
	)

	log.Printf("Main: Connecting to database at %s:%s", getEnv("DB_HOST", "localhost"), getEnv("DB_PORT", "5432"))

	db, err = gorm.Open(postgres.Open(dsn), &gorm.Config{})

	if err != nil {
		panic("failed to connect database")
	}

	log.Printf("Main: Database connected successfully")

	db.AutoMigrate(&Cosplay{})
	db.AutoMigrate(&Vote{})

	log.Printf("Main: Database migrations completed")

	// Seed random number generator
	currentCode = fmt.Sprintf("%06d", rand.Intn(1000000))
	log.Printf("Main: Initial access code generated: %s", currentCode)

	funcMap := template.FuncMap{
		"json": func(v interface{}) template.JS {
			b, err := json.Marshal(v)
			if err != nil {
				return ""
			}
			return template.JS(b)
		},
	}
	tmpl = template.New("").Funcs(funcMap)
	tmpl = template.Must(tmpl.ParseGlob("templates/**.html"))

	log.Printf("Main: Templates loaded successfully")

	log.Printf("Main: Setting up HTTP routes")

	http.HandleFunc("/", adminSessionMiddleware(serveHTML))
	http.HandleFunc("/ws", wsHandler)
	http.HandleFunc("/vote", codeSessionMiddleware(voteHandler))
	http.HandleFunc("/new-cosplay", adminSessionMiddleware(newCosplayForm))
	http.HandleFunc("/login", loginHandler)
	http.HandleFunc("/admin", adminSessionMiddleware(adminPanel))
	http.HandleFunc("/admin-controls", adminSessionMiddleware(AdminControlsHandler))
	http.HandleFunc("/admin/rank", adminSessionMiddleware(CosplayRankHandler))
	http.HandleFunc("/admin/delete-cosplay", adminSessionMiddleware(adminDeleteCosplay))
	http.HandleFunc("/admin/participants", jsonAdminMiddleware(adminParticipantsJSON))
	http.HandleFunc("/admin/upload-votes", jsonAdminMiddleware(adminUploadVotes))
	http.HandleFunc("/api/login", apiLoginHandler)
	http.HandleFunc("/check-session", checkSessionHandler)
	// Serve static files from ./static/ at /static/
	fs := http.FileServer(http.Dir("static"))
	http.Handle("/static/", http.StripPrefix("/static/", fs))

	log.Printf("Main: Starting code updater goroutine")
	go codeUpdater()

	log.Printf("Main: Server starting on port %s", addr)
	log.Printf("Main: Configuration - AllowNewSessions: %v, AllowVotes: %v, MaxVotesPerSession: %d, CodeInterval: %ds",
		allowNewSessions, allowVotes, max_votes_per_session, interval)

	log.Printf("Server running at http://localhost%s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("LoginHandler: %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)

	w.Header().Set("Content-Type", "text/html")
	if r.Method == http.MethodPost {
		// Process login
		username := r.FormValue("username")
		password := r.FormValue("password")
		log.Printf("LoginHandler: Login attempt for username: %s", username)

		if username == getEnv("ADMIN_LOGIN", "admin") && password == getEnv("ADMIN_PASSWORD", "password") {
			sessionID := uuid.New().String()
			log.Printf("LoginHandler: Successful login for %s, creating admin session: %s", username, sessionID)

			http.SetCookie(w, &http.Cookie{
				Name:  "session_id",
				Value: sessionID,
			})
			sessionsMu.Lock()
			sessions[sessionID] = Session{Type: "adm", votes: 0}
			sessionsMu.Unlock()
			http.Redirect(w, r, "/admin", http.StatusSeeOther)
			return
		} else {
			log.Printf("LoginHandler: Failed login attempt for username: %s", username)
		}
	}
	err := tmpl.ExecuteTemplate(w, "login.html", nil)
	if err != nil {
		http.Error(w, "Template execute error", http.StatusInternalServerError)
		log.Println("Template execute error:", err)
	}
}

func newCosplayForm(w http.ResponseWriter, r *http.Request) {
	log.Printf("NewCosplayForm: %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)

	w.Header().Set("Content-Type", "text/html")
	if r.Method == http.MethodPost {
		log.Printf("NewCosplayForm: Processing cosplay form submission")

		err := r.ParseMultipartForm(10 << 20)
		if err != nil {
			log.Printf("NewCosplayForm: Error parsing multipart form: %v", err)
			http.Error(w, "Deu ruim", http.StatusInternalServerError)
		}
		// Process form submission
		nome := r.FormValue("nome")
		desc := r.FormValue("desc")
		email := r.FormValue("email")
		numero := r.FormValue("numero")

		log.Printf("NewCosplayForm: Creating cosplay - Nome: %s, Desc: %s", nome, desc)

		file, handler, err := r.FormFile("foto")
		if err != nil {
			log.Printf("NewCosplayForm: Error retrieving file: %v", err)
			http.Error(w, "Error retrieving the file", http.StatusInternalServerError)
			log.Println("Error retrieving the file:", err)
			return
		}
		defer file.Close()

		log.Printf("NewCosplayForm: Processing file upload: %s", handler.Filename)

		// gen randon name to the file
		// save file to static/uploads/
		ctx := context.Background()
		new_file_name := uuid.New().String() + "_" + handler.Filename
		log.Printf("NewCosplayForm: Uploading file with new name: %s", new_file_name)

		uploader := manager.NewUploader(client)
		_, err = uploader.Upload(ctx, &s3.PutObjectInput{
			Bucket: aws.String(MINIO_BUCKET),
			Key:    aws.String(new_file_name),
			Body:   file,
		})

		if err != nil {
			log.Printf("NewCosplayForm: Error uploading file to S3: %v", err)
			http.Error(w, "Error saving the file", http.StatusInternalServerError)
			log.Println("Error saving the file:", err)
			return
		}

		log.Printf("NewCosplayForm: File uploaded successfully: %s", new_file_name)

		// TODO: change to Minio or S3
		// save to db
		cosplay := Cosplay{
			Nome:      nome,
			Desc:      desc,
			ImagePath: new_file_name,
		}
		if email != "" {
			cosplay.Email = &email
		}
		if numero != "" {
			cosplay.Numero = &numero
		}

		//_, err = dst.ReadFrom(file)

		err = gorm.G[Cosplay](db).Create(ctx, &cosplay)
		if err != nil {
			log.Printf("NewCosplayForm: Database insert error: %v", err)
			http.Error(w, "Database insert error", http.StatusInternalServerError)
			return
		}

		log.Printf("NewCosplayForm: Cosplay created successfully with ID: %d", cosplay.ID)
	}

	var data = struct {
		Created bool
	}{
		Created: r.Method == http.MethodPost,
	}
	err := tmpl.ExecuteTemplate(w, "component_cosplay_form.html", data)
	if err != nil {
		http.Error(w, "Template execute error", http.StatusInternalServerError)
		log.Println("Template execute error:", err)
	}
}

func adminDeleteCosplay(w http.ResponseWriter, r *http.Request) {
	log.Printf("AdminDeleteCosplay: %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)

	if r.Method != http.MethodPost {
		log.Printf("AdminDeleteCosplay: Method not allowed: %s", r.Method)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	idStr := r.FormValue("id")
	log.Printf("AdminDeleteCosplay: Attempting to delete cosplay with ID: %s", idStr)

	var id uint
	_, err := fmt.Sscanf(idStr, "%d", &id)
	if err != nil {
		log.Printf("AdminDeleteCosplay: Invalid ID format: %s, error: %v", idStr, err)
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	ctx := context.Background()
	_, err = gorm.G[Cosplay](db).Where("ID = ?", id).Delete(ctx)

	if err != nil {
		log.Printf("AdminDeleteCosplay: Database delete error for ID %d: %v", id, err)
		http.Error(w, "Database delete error", http.StatusInternalServerError)
		return
	}

	log.Printf("AdminDeleteCosplay: Successfully deleted cosplay with ID: %d", id)
	fmt.Fprintf(w, `<script>alert("delete")</script>`)
}

func adminPanel(w http.ResponseWriter, r *http.Request) {
	log.Printf("AdminPanel: %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)

	w.Header().Set("Content-Type", "text/html")
	ctx := context.Background()

	cosplays, err := gorm.G[Cosplay](db).Find(ctx)
	if err != nil {
		log.Printf("AdminPanel: Database query error: %v", err)
		http.Error(w, "Database query error", http.StatusInternalServerError)
		return
	}

	log.Printf("AdminPanel: Found %d cosplays", len(cosplays))

	presignClient := s3.NewPresignClient(client)

	// map cosplays to change imagePath values to respond
	for i := range cosplays {
		presignedURL, err := presignClient.PresignGetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(MINIO_BUCKET),
			Key:    aws.String(cosplays[i].ImagePath),
		}, s3.WithPresignExpires(expiry))
		if err != nil {
			log.Printf("AdminPanel: Error generating presigned URL for cosplay %d: %v", cosplays[i].ID, err)
			// http.Error(w, "Error generating image URL", http.StatusInternalServerError)
			return
		}
		cosplays[i].ImagePath = presignedURL.URL
	}

	// structo cosplays, allowcode and allowvotes
	data := struct {
		Cosplays     []Cosplay
		Allowcode    bool
		Allowvotes   bool
		Created      bool
		MaxVotes     int
		CodeInterval int
	}{
		Cosplays:     cosplays,
		Allowcode:    allowNewSessions,
		Allowvotes:   allowVotes,
		Created:      false,
		MaxVotes:     max_votes_per_session,
		CodeInterval: interval,
	}

	log.Printf("AdminPanel: Rendering admin panel with %d cosplays, AllowCode: %v, AllowVotes: %v", len(cosplays), allowNewSessions, allowVotes)

	err = tmpl.ExecuteTemplate(w, "admin.html", data)
	if err != nil {
		http.Error(w, "Template execute error", http.StatusInternalServerError)
		log.Println("Template execute error:", err)
	}

	return

}

func AdminControlsHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("AdminControlsHandler: %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)

	if r.Method != http.MethodGet {
		log.Printf("AdminControlsHandler: Method not allowed: %s", r.Method)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	action := r.URL.Query().Get("action")
	log.Printf("AdminControlsHandler: Processing action: %s", action)

	switch action {
	case "toggle_votes":
		oldValue := allowVotes
		allowVotes = !allowVotes
		log.Printf("AdminControlsHandler: Toggled votes from %v to %v", oldValue, allowVotes)
	case "toggle_sessions":
		oldValue := allowNewSessions
		allowNewSessions = !allowNewSessions
		log.Printf("AdminControlsHandler: Toggled new sessions from %v to %v", oldValue, allowNewSessions)
	case "max_votes":
		maxVotesStr := r.URL.Query().Get("value")
		var maxVotes int
		_, err := fmt.Sscanf(maxVotesStr, "%d", &maxVotes)
		if err != nil || maxVotes < 1 {
			log.Printf("AdminControlsHandler: Invalid max votes value: %s, error: %v", maxVotesStr, err)
			http.Error(w, "Invalid max votes value", http.StatusBadRequest)
			return
		}
		oldValue := max_votes_per_session
		max_votes_per_session = maxVotes
		log.Printf("AdminControlsHandler: Changed max votes per session from %d to %d", oldValue, max_votes_per_session)
	case "code_time":
		intervalStr := r.URL.Query().Get("value")
		var newInterval int
		_, err := fmt.Sscanf(intervalStr, "%d", &newInterval)
		if err != nil || newInterval < 1 {
			log.Printf("AdminControlsHandler: Invalid interval value: %s, error: %v", intervalStr, err)
			http.Error(w, "Invalid interval value", http.StatusBadRequest)
			return
		}
		oldInterval := interval
		interval = newInterval
		log.Printf("AdminControlsHandler: Changed code interval from %d to %d seconds", oldInterval, interval)
	default:
		log.Printf("AdminControlsHandler: Invalid action: %s", action)
		http.Error(w, "Invalid action", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func serveHTML(w http.ResponseWriter, r *http.Request) {
	log.Printf("ServeHTML: %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)

	w.Header().Set("Content-Type", "text/html")
	err := tmpl.ExecuteTemplate(w, "index.html", nil)
	if err != nil {
		http.Error(w, "Template execute error", http.StatusInternalServerError)
		log.Println("Template execute error:", err)
	}
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("WSHandler: WebSocket connection request from %s", r.RemoteAddr)

	c, err := upgrader.Upgrade(w, r, nil)

	if err != nil {
		log.Printf("WSHandler: WebSocket upgrade error from %s: %v", r.RemoteAddr, err)
		return
	}
	defer c.Close()

	clients[c] = true
	log.Printf("WSHandler: New WebSocket client connected, total clients: %d", len(clients))

	sendCurrentCode(c)

	for {
		_, _, err := c.ReadMessage()
		if err != nil {
			log.Printf("WSHandler: Client disconnected from %s: %v", r.RemoteAddr, err)
			delete(clients, c)
			c.Close()
			log.Printf("WSHandler: Client removed, total clients: %d", len(clients))
			break
		}
	}
}

func sendCurrentCode(c *websocket.Conn) {
	msg := fmt.Sprintf(`{"code":"%s"}`, currentCode)
	err := c.WriteMessage(websocket.TextMessage, []byte(msg))
	if err != nil {
		log.Printf("SendCurrentCode: WebSocket write error: %v", err)
		return
	}
	log.Printf("SendCurrentCode: Sent code %s to client", currentCode)
}

func CosplayRankHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("CosplayRankHandler: %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)

	w.Header().Set("Content-Type", "text/html")
	cosplays := listAllcosplayswithVotes()

	log.Printf("CosplayRankHandler: Retrieved %d cosplays with vote counts", len(cosplays))

	data := struct {
		Cosplays []CosplayWithVotes
	}{
		Cosplays: cosplays,
	}
	err := tmpl.ExecuteTemplate(w, "rank.html", data)
	if err != nil {
		log.Printf("CosplayRankHandler: Template execute error: %v", err)
		http.Error(w, "Template execute error", http.StatusInternalServerError)
		return
	}
}

func listAllcosplayswithVotes() []CosplayWithVotes {
	log.Printf("ListAllCosplaysWithVotes: Starting to retrieve cosplays with vote counts")

	var results []CosplayWithVotes
	err := db.Model(&Cosplay{}).
		Select("cosplays.*, COUNT(votes.id) as vote_count").
		Joins("LEFT JOIN votes ON votes.cosplay_voted_id = cosplays.id").
		Group("cosplays.id").
		Scan(&results).Error
	if err != nil {
		log.Printf("ListAllCosplaysWithVotes: Database query error: %v", err)
		return []CosplayWithVotes{}
	}

	log.Printf("ListAllCosplaysWithVotes: Found %d cosplays", len(results))

	for _, r := range results {
		log.Printf("ListAllCosplaysWithVotes: Cosplay: %s, Votes: %d", r.Cosplay.Desc, r.VoteCount)
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].VoteCount > results[j].VoteCount
	})

	log.Printf("ListAllCosplaysWithVotes: Sorted cosplays by vote count")

	presignClient := s3.NewPresignClient(client)
	ctx := context.Background()
	for i := range results {
		presignedURL, err := presignClient.PresignGetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(MINIO_BUCKET),
			Key:    aws.String(results[i].ImagePath),
		}, s3.WithPresignExpires(expiry))
		if err != nil {
			log.Printf("ListAllCosplaysWithVotes: Error generating presigned URL for cosplay %d: %v", results[i].ID, err)
			// http.Error(w, "Error generating image URL", http.StatusInternalServerError)
		}
		results[i].ImagePath = presignedURL.URL
	}

	log.Printf("ListAllCosplaysWithVotes: Generated presigned URLs for all cosplays")
	return results
}

func getRandomCosplayPairs(npairs int) [][2]CosplayVote {
	log.Printf("GetRandomCosplayPairs: Generating %d random cosplay pairs", npairs)

	ctx := context.Background()
	cosplays, err := gorm.G[Cosplay](db).Find(ctx)
	if err != nil {
		log.Printf("GetRandomCosplayPairs: Database query error: %v", err)
		return [][2]CosplayVote{}
	}
	if len(cosplays) < 2 {
		log.Printf("GetRandomCosplayPairs: Not enough cosplays for pairing (found %d, need at least 2)", len(cosplays))
		return [][2]CosplayVote{}
	}

	log.Printf("GetRandomCosplayPairs: Found %d cosplays total", len(cosplays))

	rand.Shuffle(len(cosplays), func(i, j int) {
		cosplays[i], cosplays[j] = cosplays[j], cosplays[i]
	})
	var maxRepeatedCosplays int = 1

	log.Printf("GetRandomCosplayPairs: Max repeated cosplays per pair generation: %d", maxRepeatedCosplays)

	pairs := make([][2]CosplayVote, 0, npairs)
	used := make(map[uint]int) // cosplay ID -> count of times used
	if npairs > len(cosplays)/2 {
	}

	for _, c := range cosplays {
		if used[c.ID] >= maxRepeatedCosplays {
			continue
		}
		for _, c2 := range cosplays {
			if c.ID != c2.ID && used[c2.ID] < maxRepeatedCosplays {
				pairs = append(pairs, [2]CosplayVote{
					{CosplayID: c.ID, Desc: c.Desc, ImagePath: c.ImagePath},
					{CosplayID: c2.ID, Desc: c2.Desc, ImagePath: c2.ImagePath},
				})
				log.Printf("GetRandomCosplayPairs: Created pair - %s vs %s", c.Desc, c2.Desc)

				used[c.ID] = used[c.ID] + 1
				used[c2.ID] = used[c2.ID] + 1
				break
			}
		}
		if len(pairs) >= npairs {
			break
		}
	}

	log.Printf("GetRandomCosplayPairs: Generated %d pairs successfully", len(pairs))
	return pairs

}

func voteHandler(w http.ResponseWriter, r *http.Request) {
	// At this point, code and session are validated and session is set in context
	log.Printf("VoteHandler: %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)

	if r.Method == http.MethodPost {
		log.Printf("VoteHandler: Processing vote submission")

		// process vote submission
		vote_str := r.FormValue("vote")
		opt1_str := r.FormValue("opt1")
		opt2_str := r.FormValue("opt2")

		log.Printf("VoteHandler: Vote data - vote: %s, opt1: %s, opt2: %s", vote_str, opt1_str, opt2_str)

		sessionCookie, err := r.Cookie("session_id")
		if err != nil || sessionCookie.Value == "" {
			log.Printf("VoteHandler: Invalid session cookie: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			tmpl.ExecuteTemplate(w, "generic_error.html", struct {
				ErrorMessage string
			}{
				ErrorMessage: "Sessão inválida",
			})
			return
		}
		sessionsMu.RLock()
		sess, ok := sessions[sessionCookie.Value]
		sessionsMu.RUnlock()
		if !ok {
			log.Printf("VoteHandler: Session not found: %s", sessionCookie.Value)
			w.WriteHeader(http.StatusBadRequest)
			tmpl.ExecuteTemplate(w, "generic_error.html", struct {
				ErrorMessage string
			}{
				ErrorMessage: "Sessão inválida",
			})
			return
		}

		log.Printf("VoteHandler: Session %s has %d votes (max: %d)", sessionCookie.Value, sess.votes, max_votes_per_session)

		if sess.votes >= max_votes_per_session {
			log.Printf("VoteHandler: Vote limit reached for session %s", sessionCookie.Value)
			http.Error(w, "Vote limit reached for this session", http.StatusForbidden)
			return
		}
		var vote, opt1, opt2 uint
		_, err = fmt.Sscanf(vote_str, "%d", &vote)
		if err != nil {
			log.Printf("VoteHandler: Invalid vote value: %s, error: %v", vote_str, err)
			http.Error(w, "Invalid vote value", http.StatusBadRequest)
			return
		}
		_, err = fmt.Sscanf(opt1_str, "%d", &opt1)
		if err != nil {
			log.Printf("VoteHandler: Invalid opt1 value: %s, error: %v", opt1_str, err)
			http.Error(w, "Invalid opt1 value", http.StatusBadRequest)
			return
		}
		_, err = fmt.Sscanf(opt2_str, "%d", &opt2)
		if err != nil {
			log.Printf("VoteHandler: Invalid opt2 value: %s, error: %v", opt2_str, err)
			http.Error(w, "Invalid opt2 value", http.StatusBadRequest)
			return
		}

		VoteCreate := Vote{
			SessionID:        sessionCookie.Value,
			CosplayOption1Id: uint(opt1),
			CosplayOption2Id: uint(opt2),
			CosplayVotedId:   uint(vote),
			Timestamp:        time.Now(),
		}

		log.Printf("VoteHandler: Creating vote record - Session: %s, Voted: %d, Options: [%d, %d]",
			VoteCreate.SessionID, VoteCreate.CosplayVotedId, VoteCreate.CosplayOption1Id, VoteCreate.CosplayOption2Id)

		ctx := context.Background()
		err = gorm.G[Vote](db).Create(ctx, &VoteCreate)
		if err != nil {
			log.Printf("VoteHandler: Database insert error: %v", err)
			http.Error(w, "Database insert error", http.StatusInternalServerError)
			return
		}

		// Update session vote count
		sessionsMu.Lock()
		sess.votes++
		sessions[sessionCookie.Value] = sess
		sessionsMu.Unlock()
		log.Printf("VoteHandler: Vote recorded successfully, session %s now has %d votes", sessionCookie.Value, sess.votes)

		// return ok js msg
		fmt.Fprintf(w, `{"status":"ok"}`)
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	log.Printf("VoteHandler: Processing vote page request")

	sessionCookie, err := r.Cookie("session_id")
	if err != nil || sessionCookie.Value == "" {
		log.Printf("VoteHandler: Invalid session cookie for GET request: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		tmpl.ExecuteTemplate(w, "generic_error.html", struct {
			ErrorMessage string
		}{
			ErrorMessage: "Sessão inválida",
		})
		return
	}
	sessionsMu.RLock()
	sess, ok := sessions[sessionCookie.Value]
	sessionsMu.RUnlock()
	if !ok {
		log.Printf("VoteHandler: Session not found for GET request: %s", sessionCookie.Value)
		w.WriteHeader(http.StatusBadRequest)
		tmpl.ExecuteTemplate(w, "generic_error.html", struct {
			ErrorMessage string
		}{
			ErrorMessage: "Sessão inválida",
		})
		return
	}
	if sess.votes >= max_votes_per_session {
		log.Printf("VoteHandler: Vote limit reached for session %s on GET request", sessionCookie.Value)
		err = tmpl.ExecuteTemplate(w, "generic_error.html", struct {
			ErrorMessage string
		}{
			ErrorMessage: "Voce alcançou o limite de votos",
		})
		w.WriteHeader(http.StatusForbidden)
		return
	}

	ctx := context.Background()

	// process vote options
	w.Header().Set("Content-Type", "text/html")
	pairs := getRandomCosplayPairs(max_votes_per_session)
	if len(pairs) == 0 {
		log.Printf("VoteHandler: No cosplay pairs available")
		w.WriteHeader(http.StatusInternalServerError)
		tmpl.ExecuteTemplate(w, "generic_error.html", struct {
			ErrorMessage string
		}{
			ErrorMessage: "Não há cosplays suficientes para votar",
		})
		return
	}

	log.Printf("VoteHandler: Generated %d cosplay pairs for voting", len(pairs))

	presignClient := s3.NewPresignClient(client)

	// Generate presigned URLs for image paths
	for i := range pairs {
		for j := range pairs[i] {
			presignedURL, err := presignClient.PresignGetObject(ctx, &s3.GetObjectInput{
				Bucket: aws.String(MINIO_BUCKET),
				Key:    aws.String(pairs[i][j].ImagePath),
			}, s3.WithPresignExpires(expiry))
			if err != nil {
				log.Printf("VoteHandler: Error generating presigned URL for pair %d, option %d: %v", i, j, err)
			}
			pairs[i][j].ImagePath = presignedURL.URL
		}
	}

	// struct with pairs and current index
	data := struct {
		Pairs     [][2]CosplayVote
		Current   int
		SessionID string
	}{
		Pairs:     pairs,
		Current:   0,
		SessionID: sessionCookie.Value,
	}

	log.Printf("VoteHandler: Rendering vote page for session %s", sessionCookie.Value)

	err = tmpl.ExecuteTemplate(w, "votes.html", data)
	if err != nil {
		http.Error(w, "Template execute error", http.StatusInternalServerError)
		log.Println("Template execute error:", err)
		return
	}
}

func adminParticipantsJSON(w http.ResponseWriter, r *http.Request) {
	ctx := context.Background()
	cosplays, err := gorm.G[Cosplay](db).Find(ctx)
	if err != nil {
		http.Error(w, "Database query error", http.StatusInternalServerError)
		return
	}

	presignClient := s3.NewPresignClient(client)
	for i := range cosplays {
		presigned, err := presignClient.PresignGetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(MINIO_BUCKET),
			Key:    aws.String(cosplays[i].ImagePath),
		}, s3.WithPresignExpires(2*time.Hour))
		if err != nil {
			log.Printf("AdminParticipantsJSON: Error generating presigned URL for cosplay %d: %v", cosplays[i].ID, err)
			http.Error(w, "Error generating image URL", http.StatusInternalServerError)
			return
		}
		cosplays[i].ImagePath = presigned.URL
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cosplays)
}

func adminUploadVotes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var input []struct {
		SessionID        string    `json:"session_id"`
		CosplayOption1Id uint      `json:"cosplay_option1_id"`
		CosplayOption2Id uint      `json:"cosplay_option2_id"`
		CosplayVotedId   uint      `json:"cosplay_voted_id"`
		Timestamp        time.Time `json:"timestamp"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	now := time.Now()
	ctx := context.Background()
	for _, v := range input {
		ts := v.Timestamp
		if ts.IsZero() {
			ts = now
		}
		vote := Vote{
			SessionID:        v.SessionID,
			CosplayOption1Id: v.CosplayOption1Id,
			CosplayOption2Id: v.CosplayOption2Id,
			CosplayVotedId:   v.CosplayVotedId,
			Timestamp:        ts,
		}
		if err := gorm.G[Vote](db).Create(ctx, &vote); err != nil {
			log.Printf("AdminUploadVotes: Database insert error: %v", err)
			http.Error(w, "Database insert error", http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","count":%d}`, len(input))
}

func adminSessionMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("AdminSessionMiddleware: %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)

		cookie, err := r.Cookie("session_id")
		if err != nil || cookie.Value == "" {
			log.Printf("AdminSessionMiddleware: No session cookie found for %s %s, error: %v", r.Method, r.URL.Path, err)
			w.WriteHeader(http.StatusUnauthorized)
			err := tmpl.ExecuteTemplate(w, "generic_error.html", struct {
				ErrorMessage string
			}{
				ErrorMessage: "Sessão não autorizada",
			})
			if err != nil {
				http.Error(w, "Template execute error", http.StatusInternalServerError)
				log.Println("Template execute error:", err)
			}
			return
		}
		sessionID := cookie.Value
		log.Printf("AdminSessionMiddleware: Found session cookie: %s", sessionID)

		sessionsMu.RLock()
		sess, ok := sessions[sessionID]
		sessionsMu.RUnlock()
		if !ok || sess.Type != "adm" {
			log.Printf("AdminSessionMiddleware: Invalid admin session for %s, session exists: %v, type: %s", sessionID, ok, sess.Type)
			w.WriteHeader(http.StatusForbidden)
			err := tmpl.ExecuteTemplate(w, "generic_error.html", struct {
				ErrorMessage string
			}{
				ErrorMessage: "Sessão não autorizada",
			})
			if err != nil {
				http.Error(w, "Template execute error", http.StatusInternalServerError)
				log.Println("Template execute error:", err)
			}
			return
		}

		log.Printf("AdminSessionMiddleware: Admin access granted for session %s", sessionID)
		next(w, r)
	}
}

// Middleware for code/session validation
func codeSessionMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("CodeSessionMiddleware: %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)

		code := r.URL.Query().Get("code")
		var sessionID string = ""

		cookie, err := r.Cookie("session_id")
		log.Printf("CodeSessionMiddleware: Code provided: %s, Cookie error: %v", code, err)

		if err == nil && cookie.Value != "" {
			sessionID = cookie.Value
			sessionsMu.RLock()
			sess, ok := sessions[sessionID]
			sessionsMu.RUnlock()
			log.Printf("CodeSessionMiddleware: Found session %s, exists: %v, type: %s, votes: %d", sessionID, ok, sess.Type, sess.votes)

			// Valid session found
			if ok && (sess.Type == "normal" || sess.Type == "adm") && allowVotes {
				log.Printf("CodeSessionMiddleware: Valid session found, allowing access")
				next(w, r)
				return
			}
		}

		if code != currentCode || !allowNewSessions {
			log.Printf("CodeSessionMiddleware: Invalid code or new sessions not allowed. Code: %s, Current: %s, AllowNew: %v", code, currentCode, allowNewSessions)
			w.Header().Set("Content-Type", "text/html")
			err := tmpl.ExecuteTemplate(w, "not_allowed.html", nil)
			if err != nil {
				http.Error(w, "Template execute error", http.StatusInternalServerError)
				log.Println("Template execute error:", err)
			}
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// Create new session if none exists and code is valid
		sessionID = uuid.New().String()
		log.Printf("CodeSessionMiddleware: Creating new session: %s", sessionID)

		http.SetCookie(w, &http.Cookie{
			Name:     "session_id",
			Value:    sessionID,
			Path:     "/",
			HttpOnly: true,
			MaxAge:   3600,
		})

		r.AddCookie(&http.Cookie{
			Name:  "session_id",
			Value: sessionID,
		})
		// Default to normal user, you can add logic to set adm type
		sessionsMu.Lock()
		sessions[sessionID] = Session{Type: "normal", votes: 0}
		sessionsMu.Unlock()
		log.Printf("CodeSessionMiddleware: New normal session created: %s", sessionID)

		next(w, r)
	}
}

func checkSessionHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	cookie, err := r.Cookie("session_id")
	if err != nil || cookie.Value == "" {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"valid":false}`)
		return
	}
	sessionsMu.RLock()
	sess, ok := sessions[cookie.Value]
	sessionsMu.RUnlock()
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"valid":false}`)
		return
	}
	remaining := max_votes_per_session - sess.votes
	fmt.Fprintf(w, `{"valid":true,"votes_remaining":%d}`, remaining)
}

func codeUpdater() {

	for {
		currentCode = fmt.Sprintf("%06d", rand.Intn(1000000))
		broadcastCode()
		time.Sleep(time.Duration(interval) * time.Second)
	}
}

func broadcastCode() {
	disconnectedClients := 0
	for c := range clients {
		msg := fmt.Sprintf(`{"code":"%s", "timeToNext": %d}`, currentCode, interval)
		err := c.WriteMessage(websocket.TextMessage, []byte(msg))
		if err != nil {
			log.Printf("BroadcastCode: Failed to send to client: %v", err)
			disconnectedClients++
		}
	}
}

func apiLoginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		fmt.Fprint(w, `{"error":"method not allowed"}`)
		return
	}
	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid JSON body"}`)
		return
	}
	if creds.Username != getEnv("ADMIN_LOGIN", "admin") || creds.Password != getEnv("ADMIN_PASSWORD", "password") {
		log.Printf("ApiLoginHandler: Failed login for username: %s", creds.Username)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"invalid credentials"}`)
		return
	}
	sessionID := uuid.New().String()
	sessionsMu.Lock()
	sessions[sessionID] = Session{Type: "adm", votes: 0}
	sessionsMu.Unlock()
	log.Printf("ApiLoginHandler: Admin session created: %s", sessionID)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"session_id":"%s"}`, sessionID)
}

// jsonAdminMiddleware checks for an admin session via cookie or Authorization: Bearer <session_id> header.
// On failure it returns JSON errors instead of HTML, making it suitable for API clients.
func jsonAdminMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var sessionID string

		// Prefer Authorization header so API clients don't need cookies.
		if auth := r.Header.Get("Authorization"); len(auth) > 7 && auth[:7] == "Bearer " {
			sessionID = auth[7:]
		} else if cookie, err := r.Cookie("session_id"); err == nil && cookie.Value != "" {
			sessionID = cookie.Value
		}

		if sessionID == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"missing session"}`)
			return
		}

		sessionsMu.RLock()
		sess, ok := sessions[sessionID]
		sessionsMu.RUnlock()
		if !ok || sess.Type != "adm" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"error":"forbidden"}`)
			return
		}

		next(w, r)
	}
}
