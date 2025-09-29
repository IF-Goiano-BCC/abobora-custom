package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"html/template"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"time"

	"encoding/json"

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

func main() {
	// Parse all templatesRat startup
	MINIO_BUCKET = getEnv("MINIO_BUCKET", "cosplay")
	MINIO_URL = getEnv("MINIO_URL", "localhost:9000/")
	var err error
	// var bucketName = MINIO_BUCKET
	var accountId = getEnv("R2_ACCOUNT_ID", "<account_id>")
	var accessKeyId = getEnv("R2_ACCESS_KEY_ID", "<access_key_id>")
	var accessKeySecret = getEnv("R2_SECRET_ACCESS_KEY", "<access_key_secret>")

	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKeyId, accessKeySecret, "")),
		config.WithRegion("auto"),
	)

	client = s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(fmt.Sprintf("https://%s.r2.cloudflarestorage.com", accountId))
	})

	if err != nil {
		log.Fatalln("Error initializing MinIO client:", err)
	}
	dsn = fmt.Sprintf("host=%s user=%s password=%s dbname=%s port=%s sslmode=disable TimeZone=UTC",
		getEnv("DB_HOST", "localhost"),
		getEnv("DB_USER", "myuser"),
		getEnv("DB_PASSWORD", " mypassword"),
		getEnv("DB_NAME", "mydb"),
		getEnv("DB_PORT", "5432"),
	)
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})

	if err != nil {
		panic("failed to connect database")
	}

	db.AutoMigrate(&Cosplay{})
	db.AutoMigrate(&Vote{})

	// Seed random number generator

	currentCode = fmt.Sprintf("%06d", rand.Intn(1000000))
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

	http.HandleFunc("/", adminSessionMiddleware(serveHTML))
	http.HandleFunc("/ws", wsHandler)
	http.HandleFunc("/vote", codeSessionMiddleware(voteHandler))
	http.HandleFunc("/new-cosplay", adminSessionMiddleware(newCosplayForm))
	http.HandleFunc("/login", loginHandler)
	http.HandleFunc("/admin", adminSessionMiddleware(adminPanel))
	http.HandleFunc("/admin-controls", adminSessionMiddleware(AdminControlsHandler))
	http.HandleFunc("/admin/rank", adminSessionMiddleware(CosplayRankHandler))
	http.HandleFunc("/admin/delete-cosplay", adminSessionMiddleware(adminDeleteCosplay))
	// Serve static files from ./static/ at /static/
	fs := http.FileServer(http.Dir("static"))
	http.Handle("/static/", http.StripPrefix("/static/", fs))

	go codeUpdater()

	log.Printf("Server running at http://localhost%s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	if r.Method == http.MethodPost {
		// Process login
		username := r.FormValue("username")
		password := r.FormValue("password")
		if username == getEnv("ADMIN_LOGIN", "admin") && password == getEnv("ADMIN_PASSWORD", "password") {
			sessionID := uuid.New().String()
			http.SetCookie(w, &http.Cookie{
				Name:  "session_id",
				Value: sessionID,
			})
			sessions[sessionID] = Session{Type: "adm", votes: 0}
			http.Redirect(w, r, "/admin", http.StatusSeeOther)
			return
		}
	}
	err := tmpl.ExecuteTemplate(w, "login.html", nil)
	if err != nil {
		http.Error(w, "Template execute error", http.StatusInternalServerError)
		log.Println("Template execute error:", err)
	}
}

func newCosplayForm(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	if r.Method == http.MethodPost {
		err := r.ParseMultipartForm(10 << 20)
		if err != nil {
			http.Error(w, "Deu ruim", http.StatusInternalServerError)
		}
		// Process form submission
		nome := r.FormValue("nome")
		desc := r.FormValue("desc")
		email := r.FormValue("email")
		numero := r.FormValue("numero")

		file, handler, err := r.FormFile("foto")
		if err != nil {
			http.Error(w, "Error retrieving the file", http.StatusInternalServerError)
			log.Println("Error retrieving the file:", err)
			return
		}
		defer file.Close()
		// gen randon name to the file
		// save file to static/uploads/
		ctx := context.Background()
		new_file_name := uuid.New().String() + "_" + handler.Filename
		uploader := manager.NewUploader(client)
		_, err = uploader.Upload(ctx, &s3.PutObjectInput{
			Bucket: aws.String(MINIO_BUCKET),
			Key:    aws.String(new_file_name),
			Body:   file,
		})

		if err != nil {
			http.Error(w, "Error saving the file", http.StatusInternalServerError)
			log.Println("Error saving the file:", err)
			return
		}

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

		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
		if err != nil {
			http.Error(w, "Database connection error", http.StatusInternalServerError)
			return
		}

		err = gorm.G[Cosplay](db).Create(ctx, &cosplay)
		if err != nil {
			http.Error(w, "Database insert error", http.StatusInternalServerError)
			return
		}
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
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	idStr := r.FormValue("id")
	var id uint
	_, err := fmt.Sscanf(idStr, "%d", &id)
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		http.Error(w, "Database connection error", http.StatusInternalServerError)
		return
	}
	ctx := context.Background()
	_, err = gorm.G[Cosplay](db).Where("ID = ?", id).Delete(ctx)

	if err != nil {
		http.Error(w, "Database delete error", http.StatusInternalServerError)
		return
	}

	fmt.Fprintf(w, `<script>alert("delete")</script>`)
}

func adminPanel(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})

	if err != nil {
		http.Error(w, "Database connection error", http.StatusInternalServerError)
		return
	}

	ctx := context.Background()

	cosplays, err := gorm.G[Cosplay](db).Find(ctx)
	presignClient := s3.NewPresignClient(client)

	// map cosplays to change imagePath values to respond
	for i := range cosplays {
		presignedURL, err := presignClient.PresignGetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(MINIO_BUCKET),
			Key:    aws.String(cosplays[i].ImagePath),
		}, s3.WithPresignExpires(expiry))
		if err != nil {
			log.Println("Error generating presigned URL:", err)
			// http.Error(w, "Error generating image URL", http.StatusInternalServerError)
			return
		}
		cosplays[i].ImagePath = presignedURL.URL
	}
	if err != nil {
		http.Error(w, "Database query error", http.StatusInternalServerError)
		return
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

	err = tmpl.ExecuteTemplate(w, "admin.html", data)
	if err != nil {
		http.Error(w, "Template execute error", http.StatusInternalServerError)
		log.Println("Template execute error:", err)
	}

	return

}

func AdminControlsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	action := r.URL.Query().Get("action")

	switch action {
	case "toggle_votes":
		allowVotes = !allowVotes
	case "toggle_sessions":
		allowNewSessions = !allowNewSessions
	case "max_votes":
		maxVotesStr := r.URL.Query().Get("value")
		var maxVotes int
		_, err := fmt.Sscanf(maxVotesStr, "%d", &maxVotes)
		if err != nil || maxVotes < 1 {
			http.Error(w, "Invalid max votes value", http.StatusBadRequest)
			return
		}
		max_votes_per_session = maxVotes
		log.Println("Max votes per session set to:", max_votes_per_session)
	case "code_time":
		intervalStr := r.URL.Query().Get("value")
		var newInterval int
		_, err := fmt.Sscanf(intervalStr, "%d", &newInterval)
		if err != nil || newInterval < 1 {
			http.Error(w, "Invalid interval value", http.StatusBadRequest)
			return
		}
		log.Println("Code interval changed from", interval, "to", newInterval)
		interval = newInterval
	default:
		http.Error(w, "Invalid action", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func serveHTML(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	err := tmpl.ExecuteTemplate(w, "index.html", nil)
	if err != nil {
		http.Error(w, "Template execute error", http.StatusInternalServerError)
		log.Println("Template execute error:", err)
	}
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	c, err := upgrader.Upgrade(w, r, nil)

	if err != nil {
		log.Println("WebSocket upgrade error:", err)
		return
	}
	defer c.Close()

	clients[c] = true
	sendCurrentCode(c)

	for {
		_, _, err := c.ReadMessage()
		if err != nil {
			delete(clients, c)
			c.Close()
			break
		}
	}
}

func sendCurrentCode(c *websocket.Conn) {

	msg := fmt.Sprintf(`{"code":"%s"}`, currentCode)
	err := c.WriteMessage(websocket.TextMessage, []byte(msg))
	if err != nil {
		log.Println("WebSocket write error:", err)
		return
	}
}

func CosplayRankHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	cosplays := listAllcosplayswithVotes()
	data := struct {
		Cosplays []CosplayWithVotes
	}{
		Cosplays: cosplays,
	}
	err := tmpl.ExecuteTemplate(w, "rank.html", data)
	if err != nil {
		http.Error(w, "Template execute error", http.StatusInternalServerError)
		log.Println("Template execute error:", err)
		return
	}
}

func listAllcosplayswithVotes() []CosplayWithVotes {
	// list all cosplays with votes count
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Println("Database connection error:", err)
		return []CosplayWithVotes{}
	}
	var results []CosplayWithVotes
	err = db.Model(&Cosplay{}).
		Select("cosplays.*, COUNT(votes.id) as vote_count").
		Joins("LEFT JOIN votes ON votes.cosplay_voted_id = cosplays.id").
		Group("cosplays.id").
		Scan(&results).Error
	if err != nil {
		log.Println("Database query error:", err)
		return []CosplayWithVotes{}
	}
	for _, r := range results {
		log.Printf("Cosplay: %s, Votes: %d\n", r.Cosplay.Desc, r.VoteCount)
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].VoteCount > results[j].VoteCount
	})
	presignClient := s3.NewPresignClient(client)
	ctx := context.Background()
	for i := range results {
		presignedURL, err := presignClient.PresignGetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(MINIO_BUCKET),
			Key:    aws.String(results[i].ImagePath),
		}, s3.WithPresignExpires(expiry))
		if err != nil {
			log.Println("Error generating presigned URL:", err)
			// http.Error(w, "Error generating image URL", http.StatusInternalServerError)
		}
		results[i].ImagePath = presignedURL.URL
	}

	return results
}

func getRandomCosplayPairs(npairs int) [][2]CosplayVote {
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Println("Database connection error:", err)
		return [][2]CosplayVote{}
	}
	ctx := context.Background()
	cosplays, err := gorm.G[Cosplay](db).Find(ctx)
	if err != nil {
		log.Println("Database query error:", err)
		return [][2]CosplayVote{}
	}
	if len(cosplays) < 2 {
		return [][2]CosplayVote{}
	}
	rand.Shuffle(len(cosplays), func(i, j int) {
		cosplays[i], cosplays[j] = cosplays[j], cosplays[i]
	})
	var maxRepeatedCosplays int
	if len(cosplays)%2 == 0 {
		maxRepeatedCosplays = len(cosplays) / 2
	} else {
		maxRepeatedCosplays = (len(cosplays) - 1) / 2
	}
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
				fmt.Println("Pair:", c.Desc, c2.Desc, c)

				used[c.ID] = used[c.ID] + 1
				used[c2.ID] = used[c2.ID] + 1
				break
			}
		}
		if len(pairs) >= npairs {
			break
		}
	}
	return pairs

}

func voteHandler(w http.ResponseWriter, r *http.Request) {
	// At this point, code and session are validated and session is set in context

	if r.Method == http.MethodPost {
		// process vote submission
		vote_str := r.FormValue("vote")
		opt1_str := r.FormValue("opt1")
		opt2_str := r.FormValue("opt2")
		sessionCookie, err := r.Cookie("session_id")
		if err != nil || sessionCookie.Value == "" {
			w.WriteHeader(http.StatusBadRequest)
			tmpl.ExecuteTemplate(w, "generic_error.html", struct {
				ErrorMessage string
			}{
				ErrorMessage: "Sessão inválida",
			})
			return
		}
		sess, ok := sessions[sessionCookie.Value]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			tmpl.ExecuteTemplate(w, "generic_error.html", struct {
				ErrorMessage string
			}{
				ErrorMessage: "Sessão inválida",
			})
			return
		}
		if sess.votes >= max_votes_per_session {
			http.Error(w, "Vote limit reached for this session", http.StatusForbidden)
			return
		}
		var vote, opt1, opt2 uint
		_, err = fmt.Sscanf(vote_str, "%d", &vote)
		if err != nil {
			http.Error(w, "Invalid vote value", http.StatusBadRequest)
			return
		}
		_, err = fmt.Sscanf(opt1_str, "%d", &opt1)
		if err != nil {
			http.Error(w, "Invalid opt1 value", http.StatusBadRequest)
			return
		}
		_, err = fmt.Sscanf(opt2_str, "%d", &opt2)
		if err != nil {
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
		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
		if err != nil {
			http.Error(w, "Database connection error", http.StatusInternalServerError)
			return
		}

		ctx := context.Background()
		err = gorm.G[Vote](db).Create(ctx, &VoteCreate)
		if err != nil {
			http.Error(w, "Database insert error", http.StatusInternalServerError)
			return
		}
		// return ok js msg
		fmt.Fprintf(w, `{"status":"ok"}`)
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sessionCookie, err := r.Cookie("session_id")
	if err != nil || sessionCookie.Value == "" {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Printf("err: %v", err)
		fmt.Printf("err: cookie %v", sessionCookie.Value)
		tmpl.ExecuteTemplate(w, "generic_error.html", struct {
			ErrorMessage string
		}{
			ErrorMessage: "Sessão inválida",
		})
		return
	}
	sess, ok := sessions[sessionCookie.Value]
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Printf("sesssions err: %v", err)
		fmt.Printf("err: cookie %v", sessionCookie.Value)
		tmpl.ExecuteTemplate(w, "generic_error.html", struct {
			ErrorMessage string
		}{
			ErrorMessage: "Sessão inválida",
		})
		return
	}
	if sess.votes >= max_votes_per_session {
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
		w.WriteHeader(http.StatusInternalServerError)
		tmpl.ExecuteTemplate(w, "generic_error.html", struct {
			ErrorMessage string
		}{
			ErrorMessage: "Não há cosplays suficientes para votar",
		})
		return
	}
	presignClient := s3.NewPresignClient(client)

	// Generate presigned URLs for image paths
	for i := range pairs {
		for j := range pairs[i] {
			presignedURL, err := presignClient.PresignGetObject(ctx, &s3.GetObjectInput{
				Bucket: aws.String(MINIO_BUCKET),
				Key:    aws.String(pairs[i][j].ImagePath),
			}, s3.WithPresignExpires(expiry))
			if err != nil {
				log.Println("Error generating presigned URL:", err)
			}
			pairs[i][j].ImagePath = presignedURL.URL
		}
	}

	// struct with pairs and current index
	data := struct {
		Pairs   [][2]CosplayVote
		Current int
	}{
		Pairs:   pairs,
		Current: 0,
	}
	err = tmpl.ExecuteTemplate(w, "votes.html", data)
	if err != nil {
		http.Error(w, "Template execute error", http.StatusInternalServerError)
		log.Println("Template execute error:", err)
		return
	}
}

func adminSessionMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("session_id")
		if err != nil || cookie.Value == "" {
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
		sess, ok := sessions[sessionID]
		if !ok || sess.Type != "adm" {
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
		next(w, r)
	}
}

// Middleware for code/session validation
func codeSessionMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		var sessionID string = ""

		cookie, err := r.Cookie("session_id")
		fmt.Println("erro", err)
		fmt.Println("code", cookie)

		if err == nil && cookie.Value != "" {
			sess, ok := sessions[sessionID]
			fmt.Println("sess", sess)
			fmt.Println("ok", ok)
			// Valid session found
			if ok && (sess.Type == "normal" || sess.Type == "adm") && allowVotes {
				next(w, r)
				return
			}
		}

		if code != currentCode && !allowNewSessions {
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
		if err != nil || cookie.Value == "" {
			sessionID = uuid.New().String()
			http.SetCookie(w, &http.Cookie{
				Name:     "session_id",
				Value:    sessionID,
				Path:     "/",
				HttpOnly: true,
				MaxAge:   3600,
			})
			// Default to normal user, you can add logic to set adm type
			sessions[sessionID] = Session{Type: "normal", votes: 0}
		}

		next(w, r)
	}
}

func codeUpdater() {
	for {
		currentCode = fmt.Sprintf("%06d", rand.Intn(1000000))
		broadcastCode()
		time.Sleep(time.Duration(interval) * time.Second)
	}
}

func broadcastCode() {
	for c := range clients {
		// url := getEnv("APP_DOMAIN", "http://localhost:8080/") // You may want to make this dynamic
		sendCurrentCode(c)
	}
}

func encodeBase64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}
