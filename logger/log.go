package logger

import (
	"fmt"
	"os"
	"sync"
	"time"
)

var (
	mu   sync.Mutex
	file *os.File
)

func Init(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	mu.Lock()
	file = f
	mu.Unlock()
	Log("logger started, path=%s", path)
	return nil
}

func Log(format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	if file == nil {
		return
	}
	ts := time.Now().Format("15:04:05.000")
	fmt.Fprintf(file, "[%s] %s\n", ts, fmt.Sprintf(format, args...))
}

func Close() {
	mu.Lock()
	defer mu.Unlock()
	if file != nil {
		file.Close()
		file = nil
	}
}
