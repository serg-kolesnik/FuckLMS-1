// FuckLMS attendance service: CRUD студентов в PostgreSQL и параллельная
// отметка посещаемости в Moodle LMS с SSE-стримом результатов (ADR 0001).
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// app — общий контекст обработчиков.
type app struct {
	cfg   Config
	store *Store
}

func main() {
	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("конфигурация: %v", err)
	}

	store, err := NewStore(context.Background(), cfg.DSN)
	if err != nil {
		log.Fatalf("подключение к PostgreSQL: %v", err)
	}
	defer store.Close()

	a := &app{cfg: cfg, store: store}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", a.handleHealth)
	mux.HandleFunc("GET /api/students", a.auth(a.handleListStudents))
	mux.HandleFunc("POST /api/students", a.auth(a.handleCreateStudent))
	mux.HandleFunc("PUT /api/students/{id}", a.auth(a.handleUpdateStudent))
	mux.HandleFunc("DELETE /api/students/{id}", a.auth(a.handleDeleteStudent))
	mux.HandleFunc("POST /api/mark", a.auth(a.handleMark))

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Мягкое завершение по SIGINT/SIGTERM: останавливаем приём соединений,
	// даём активной отметке до 30 секунд закончиться.
	done := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Println("получен сигнал завершения, останавливаю сервер...")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		close(done)
	}()

	log.Printf("FuckLMS service слушает %s (параллельность отметки: %d)",
		cfg.Addr, cfg.Concurrency)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("HTTP-сервер: %v", err)
	}
	<-done
}
