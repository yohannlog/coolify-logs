package logs

import (
	"github.com/gin-gonic/gin"
	"log"
	"net/http"
	"os"
)

var logsPath = "/app/logs"
var version string = "0.0.1"
var logsDir string = "/app/logs"

const maxLogFileSize = 1024 * 1024 * 10 // 10MB, change this to your desired max size

const logsFile = "/app/logs/{container_id}/{date}.csv"

// Arguments
var token string

func Token() gin.HandlerFunc {
	return func(c *gin.Context) {
		if token != "" {
			if c.GetHeader("Authorization") != "Bearer "+token {
				c.JSON(401, gin.H{
					"error": "Unauthorized",
				})
				c.Abort()
				return
			}
		}
		c.Next()
	}
}

type LogInput struct {
	Log string `json:"log"`
}

func main() {
	if gin.Mode() == gin.DebugMode {
		logsPath = "./logs"
	}

	if err := os.MkdirAll(logsPath, 0700); err != nil {
		log.Fatalf("Error creating logs directory: %v", err)
	}

	r := gin.Default()
	r.GET("/api/health", func(c *gin.Context) {
		c.String(200, "OK")
	})
	r.GET("/api/version", func(c *gin.Context) {
		c.String(200, version)
	})
	r.Use(gin.Recovery())

	err := r.Run("0.0.0.0:8889")
	if err != nil {
		return
	}

	authorized := r.Group("/api")
	authorized.Use(Token())

	go func() {
		if err := streamLogsToFile(); err != nil {
			log.Fatalf("Error listening to events: %v", err)
		}
	}()

	authorized.GET("/:containerId", func(c *gin.Context) {
		containerId := c.Param("containerId")
		lineRange := c.Query("lineRange")
		dateRange := c.Query("dateRange")

		logs, err := getContainerLogs(containerId, lineRange, dateRange)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"logs": logs})
	})

}
