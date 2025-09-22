package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"time"
  "math/rand"

	"github.com/gofiber/fiber/v2"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/v2/bson"
	"github.com/gofiber/template/html/v2"
)

func ConnectDB() *mongo.Client {
	client, err := mongo.NewClient(options.Client().ApplyURI("mongodb://admin:password@127.0.0.1:27017/")) // Replace with your MongoDB URI
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = client.Connect(ctx)
	if err != nil {
		log.Fatal(err)
	}

	// Ping the database to check the connection
	err = client.Ping(ctx, nil)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("Connected to MongoDB!")
	return client
}	 

type Fantasia struct {    
	ID primitive.ObjectID `bson:"_id" json:"_id"`
	Nome_participante string `bson:"nome_participante" json:"nome_participante"`
	Nome_fantasia string `bson:"nome_fantasia" json:"nome_fantasia"`
	Descricao string `bson:"descricao_fantasia" json:"descricao"`
	UrlFoto string `bson:"url_foto" json:"url_foto"`
	Votos int `bson:"votos" json:"votos"`
}

type FantasiaResponse struct {    
	ID string `bson:"_id" json:"_id"`
	Nome_participante string `bson:"nome_participante" json:"nome_participante"`
	Nome_fantasia string `bson:"nome_fantasia" json:"nome_fantasia"`
	Descricao string `bson:"descricao_fantasia" json:"descricao"`
	UrlFoto string `bson:"url_foto" json:"url_foto"`
	Votos int `bson:"votos" json:"votos"`
}

func main() { 
	minioClient, err := minio.New("localhost:9000", &minio.Options{
		Creds:  credentials.NewStaticV4("minioadmin", "password",  ""),
		Secure: false,
	 })

	if err != nil {
		log.Fatalln("Error initialize minio client: ", err)
	}

	client := ConnectDB()
	database := client.Database("database")
	fantasiaCollection := database.Collection("fantasia")

	engine := html.New("./templates", ".html")

	app := fiber.New(fiber.Config{
		Views: engine,
	})

	app.Get("/", func (c *fiber.Ctx) error {
		return c.Render("home", fiber.Map {})
	})

	app.Get("/admin", func (c *fiber.Ctx) error {
		return c.Render("admin", fiber.Map {})
	})

	app.Get("/login", func (c *fiber.Ctx) error {
		return c.Render("login", fiber.Map {})
	})

	app.Get("/fantasia", func (c *fiber.Ctx) error {
		return c.Render("fantasia", fiber.Map { "isAlert": "" })
	})

	app.Get("/galeria", func (c *fiber.Ctx) error {
		cursor, err := fantasiaCollection.Find(context.TODO(),	bson.M {})

		if err != nil {
			fmt.Println(err)
		}

		var result []FantasiaResponse

		if err := cursor.All(context.TODO(), &result); err != nil {
			fmt.Println(err)
		}
		
		return c.Render("galeria", fiber.Map { "fantasias": result })
	})

	app.Get("/votacao", func (c *fiber.Ctx) error {
		cursor, err := fantasiaCollection.Find(context.TODO(),	bson.M {})

		if err != nil {
			fmt.Println(err)
		}

		var result []FantasiaResponse

		if err := cursor.All(context.TODO(), &result); err != nil {
			fmt.Println(err)
		}
		
		return c.Render("votacao", fiber.Map { "fantasias": result })
	})

	app.Post("/fantasia-form", func (c * fiber.Ctx) error {
		fantasia := Fantasia {}

		fantasia.Nome_participante = c.FormValue("nome")
		fantasia.Nome_fantasia = c.FormValue("nomeFantasia")
		fantasia.Descricao = c.FormValue("descricao")
		fantasia.Votos = 0
		
		if fantasia.Nome_participante == "" || fantasia.Nome_fantasia == "" {
			return c.SendString("forms error")
		}

		foto, err := c.FormFile("foto")

		if err != nil {
			return c.Render("fantasia", fiber.Map { "isAlert": "falha" })
		}

		extension := filepath.Ext(foto.Filename)

		fileContent, err := foto.Open()

		if err != nil {
			return c.Render("fantasia", fiber.Map { "isAlert": "falha" })
		}
		
		defer fileContent.Close()

		buffer := bytes.NewBuffer(nil)
		
		if _, err := io.Copy(buffer, fileContent); err != nil {
			return c.Render("fantasia", fiber.Map { "isAlert": "falha" })
		}

		fileBytes := buffer.Bytes()
		fileReader := bytes.NewReader(fileBytes)

		fantasia.ID = primitive.NewObjectID()

		nomeArquivo := fantasia.ID.Hex() + extension

		_, err = minioClient.PutObject(context.Background(), "fantasias", nomeArquivo, fileReader, int64(len(fileBytes)), minio.PutObjectOptions{
			ContentType: "application/octet-stream", // Set appropriate Content-Type
		})

		if err != nil {
			fmt.Println(err)		
			return c.Render("fantasia", fiber.Map { "isAlert": "falha" })
		}

		fantasia.UrlFoto = "/image/" + nomeArquivo

		_, err = fantasiaCollection.InsertOne(context.TODO(), fantasia)

		if _, err := io.Copy(buffer, fileContent); err != nil {
			return c.Render("fantasia", fiber.Map { "isAlert": "falha" })
		}

		return c.Render("fantasia", fiber.Map { "isAlert": "sucesso" })
	})

	app.Get("/image/:id", func (c *fiber.Ctx) error {
		imageID := c.Params("id")

		object, err := minioClient.GetObject(context.Background(), "fantasias", imageID, minio.GetObjectOptions{})

		if err != nil {
			log.Fatalln(err)
		}

		defer object.Close()
	
		buffer := bytes.NewBuffer(nil)
		
		if _, err := io.Copy(buffer, object); err != nil {
			return c.SendString("erro")
		}

		reader := bytes.NewReader(buffer.Bytes())

		ext := filepath.Ext(imageID)

		if len(ext) > 0 {
			c.Set("Content-Type", "image/" + ext[1:])
		}
		return c.SendStream(reader)
	})

	app.Get("/list_all_participants", func (c *fiber.Ctx) error {
		cursor, _ := fantasiaCollection.Find(context.TODO(),	bson.M {})

		var result []FantasiaResponse

		cursor.All(context.TODO(), &result) 

    if c.Query("votacao") == "true" {
      source := rand.NewSource(time.Now().UnixNano())
      random := rand.New(source)
      for i := len(result) - 1; i > 0; i-- {
          j := random.Intn(i + 1)
          result[i], result[j] = result[j], result[i]
      }
      return c.JSON(result[:min(len(result), 8)])
    }

		return c.JSON(result)
	})

	app.Post("/votar/:id", func (c *fiber.Ctx) error {
		fantasiaID, _ := primitive.ObjectIDFromHex(c.Params("id"))
		result, err := fantasiaCollection.UpdateByID(context.TODO(), fantasiaID, bson.M {"$inc": bson.M { "votos": 1}})
	
		if err != nil {
			return c.JSON("{ erro: \"erro\"}")
		}

		return c.JSON(result)
	})

	app.Listen(":8000")
}
