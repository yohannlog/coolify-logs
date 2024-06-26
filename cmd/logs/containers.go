package logs

import (
	"bufio"
	_ "bufio"
	"bytes"
	_ "bytes"
	"context"
	"fmt"
	"github.com/docker/docker/api/types/events"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

var wg sync.WaitGroup

func streamLogsToFile() error {
	ctx := context.Background()
	apiClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer func(apiClient *client.Client) {
		err := apiClient.Close()
		if err != nil {
			fmt.Println("Error closing Docker client:", err)
		}
	}(apiClient)

	go listenToContainerCreation(ctx, apiClient)

	containers, err := apiClient.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		return err
	}

	for _, cont := range containers {
		attachContainer(cont, ctx, apiClient)
	}
	wg.Wait()

	return nil
}

func attachContainer(cont types.Container, ctx context.Context, apiClient *client.Client) {
	wg.Add(1)
	go func(cont types.Container) {
		defer wg.Done()
		name := cont.Labels["coolify.name"]
		if name == "" {
			name = cont.Names[0][1:]
		} else {
			if cont.Labels["coolify.pullRequestId"] != "0" {
				name = fmt.Sprintf("%s-pr-%s", name, cont.Labels["coolify.pullRequestId"])
			}
		}
		containerDir := fmt.Sprintf("%s/%s", logsDir, name)
		if _, err := os.Stat(containerDir); os.IsNotExist(err) {
			err := os.MkdirAll(containerDir, 0700)
			if err != nil {
				fmt.Printf("Error creating directory %s: %s\n", containerDir, err)
				return
			}
		}
		currentDate := time.Now().Format("2006-01-02")
		logFileName := fmt.Sprintf("%s/%s.txt", containerDir, currentDate)

		streamLogs(ctx, apiClient, cont, logFileName)
	}(cont)
}

func openLogFile(logFileName string) (*os.File, error) {
	suffix := 0
	var logFile *os.File
	var err error

	for {
		fileName := logFileName
		if suffix > 0 {
			fileName = fmt.Sprintf("%s(%d)", logFileName, suffix)
		}

		logFile, err = os.OpenFile(fileName, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0666)
		if err != nil {
			return nil, err
		}

		info, err := logFile.Stat()
		if err != nil {
			err := logFile.Close()
			if err != nil {
				return nil, err
			}
			return nil, err
		}

		if info.Size() < maxLogFileSize {
			break
		}

		err = logFile.Close()
		if err != nil {
			return nil, err
		}
		suffix++
	}

	return logFile, nil
}

func streamLogs(ctx context.Context, apiClient *client.Client, cont types.Container, logFileName string) {
	out, err := apiClient.ContainerLogs(ctx, cont.ID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Tail:       "all",
		Timestamps: true,
	})
	if err != nil {
		fmt.Printf("Error retrieving logs for container %s: %s\n", cont.ID, err)
		return
	}
	defer func(out io.ReadCloser) {
		err := out.Close()
		if err != nil {
			fmt.Printf("Error closing logs stream for container %s: %s\n", cont.ID, err)
		}
	}(out)

	logFile, err := openLogFile(logFileName)
	if err != nil {
		fmt.Printf("Error opening log file %s: %s\n", logFileName, err)
		return
	}
	defer func(logFile *os.File) {
		err := logFile.Close()
		if err != nil {
			fmt.Printf("Error closing log file %s: %s\n", logFileName, err)
		}
	}(logFile)

	seenLines := make(map[string]bool)
	re := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	stdOutWriter := newRemovingWriter(logFile, seenLines, re)
	stdErrWriter := newRemovingWriter(logFile, seenLines, re)

	if _, err := stdcopy.StdCopy(stdOutWriter, stdErrWriter, out); err != nil {
		fmt.Printf("Error saving logs for container %s: %s\n", cont.ID, err)
	}
}

type removingWriter struct {
	file      *os.File
	seenLines map[string]bool
	regex     *regexp.Regexp
}

func newRemovingWriter(file *os.File, seen map[string]bool, regex *regexp.Regexp) *removingWriter {
	return &removingWriter{
		file:      file,
		seenLines: seen,
		regex:     regex,
	}
}

func (rw *removingWriter) Write(p []byte) (n int, err error) {
	scanner := bufio.NewScanner(bytes.NewReader(p))
	for scanner.Scan() {
		line := scanner.Text()
		cleanLine := rw.regex.ReplaceAllString(line, "") // Remove ANSI escape codes
		if !rw.seenLines[cleanLine] {
			rw.seenLines[cleanLine] = true
			if _, err := fmt.Fprintln(rw.file, cleanLine); err != nil {
				return 0, err
			}
		}
	}
	return len(p), scanner.Err()
}

func listenToContainerCreation(ctx context.Context, apiClient *client.Client) {
	messages, errs := apiClient.Events(ctx, events.ListOptions{})
	for {
		select {
		case err := <-errs:
			if err != nil {
				fmt.Println("Error listening to Docker events:", err)
				return
			}
		case msg := <-messages:
			if msg.Type == "container" && msg.Action == "create" {
				containers, err := apiClient.ContainerList(ctx, container.ListOptions{})
				if err != nil {
					fmt.Println("Error listing containers:", err)
					return
				}
				for _, cont := range containers {
					if cont.ID == msg.Actor.ID {
						if cont.Image == "ghcr.io/coollabsio/coolify-helper:latest" {
							continue
						}
						if cont.Names[0] == "/coolify-logs" {
							continue
						}
						attachContainer(cont, ctx, apiClient)
						break
					}
				}
			}
		}
	}
}

func getContainerLogs(containerId string, startTime string, endTime string) ([]string, error) {
	containerDir := filepath.Join(logsDir, containerId)
	files, err := os.ReadDir(containerDir)
	if err != nil {
		return nil, err
	}

	var logs []string
	for _, file := range files {
		if !file.IsDir() && strings.HasSuffix(file.Name(), ".txt") {
			filePath := filepath.Join(containerDir, file.Name())
			fileLogs, err := readLogFile(filePath, startTime, endTime)
			if err != nil {
				return nil, err
			}
			logs = append(logs, fileLogs...)
		}
	}

	return logs, nil
}

func readLogFile(filePath string, startTime string, endTime string) ([]string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer func(file *os.File) {
		err := file.Close()
		if err != nil {
			fmt.Printf("Error closing file %s: %s\n", filePath, err)
		}
	}(file)

	start, _ := time.Parse(time.RFC3339, startTime)
	end, _ := time.Parse(time.RFC3339, endTime)

	scanner := bufio.NewScanner(file)
	var logs []string
	for scanner.Scan() {
		line := scanner.Text()

		// assuming the timestamp is at the beginning of the line in RFC3339 format
		logTime, _ := time.Parse(time.RFC3339, strings.Split(line, " ")[0])

		if logTime.Before(start) || logTime.After(end) {
			continue
		}

		logs = append(logs, line)
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return logs, nil
}
