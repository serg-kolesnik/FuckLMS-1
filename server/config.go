package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config — параметры сервиса. Все задаются переменными окружения,
// полный список — в server/README.md.
type Config struct {
	Addr        string // адрес HTTP-сервера (PORT)
	Token       string // bearer-токен для API (TOKEN)
	DSN         string // строка подключения к PostgreSQL (DATABASE_URL)
	Concurrency int    // лимит параллельной отметки (MARK_CONCURRENCY)
	LMSBaseURL  string // базовый URL Moodle (LMS_BASE_URL)
}

// LoadConfig читает переменные окружения и валидирует обязательные.
func LoadConfig() (Config, error) {
	cfg := Config{
		Addr:        addrFromPort(os.Getenv("PORT")),
		Token:       os.Getenv("TOKEN"),
		DSN:         os.Getenv("DATABASE_URL"),
		LMSBaseURL:  envDefault("LMS_BASE_URL", "https://lms.uni-dubna.ru"),
		Concurrency: 5,
	}

	if v := os.Getenv("MARK_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return Config{}, fmt.Errorf("MARK_CONCURRENCY: ожидалось положительное число, получено %q", v)
		}
		cfg.Concurrency = n
	}

	if cfg.Token == "" {
		return Config{}, errors.New("TOKEN: не задан bearer-токен для API")
	}
	if cfg.DSN == "" {
		return Config{}, errors.New("DATABASE_URL: не задана строка подключения к PostgreSQL")
	}
	return cfg, nil
}

// addrFromPort превращает "8080" в ":8080"; значение с портом (":8080") проходит как есть.
func addrFromPort(port string) string {
	port = strings.TrimSpace(port)
	if port == "" {
		return ":8080"
	}
	if strings.Contains(port, ":") {
		return port
	}
	return ":" + port
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
