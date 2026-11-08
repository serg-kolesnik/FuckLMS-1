package main

import (
	"testing"
)

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    Config
		wantErr bool
	}{
		{
			name: "минимальный набор переменных — значения по умолчанию",
			env:  map[string]string{"TOKEN": "t", "DATABASE_URL": "postgres://db"},
			want: Config{
				Addr:        ":8080",
				Token:       "t",
				DSN:         "postgres://db",
				Concurrency: 5,
				LMSBaseURL:  "https://lms.uni-dubna.ru",
			},
		},
		{
			name:    "нет TOKEN",
			env:     map[string]string{"DATABASE_URL": "postgres://db"},
			wantErr: true,
		},
		{
			name:    "нет DATABASE_URL",
			env:     map[string]string{"TOKEN": "t"},
			wantErr: true,
		},
		{
			name: "MARK_CONCURRENCY переопределён",
			env: map[string]string{
				"TOKEN": "t", "DATABASE_URL": "postgres://db", "MARK_CONCURRENCY": "3",
			},
			want: Config{
				Addr: ":8080", Token: "t", DSN: "postgres://db",
				Concurrency: 3, LMSBaseURL: "https://lms.uni-dubna.ru",
			},
		},
		{
			name: "MARK_CONCURRENCY ноль",
			env: map[string]string{
				"TOKEN": "t", "DATABASE_URL": "postgres://db", "MARK_CONCURRENCY": "0",
			},
			wantErr: true,
		},
		{
			name: "MARK_CONCURRENCY отрицательный",
			env: map[string]string{
				"TOKEN": "t", "DATABASE_URL": "postgres://db", "MARK_CONCURRENCY": "-2",
			},
			wantErr: true,
		},
		{
			name: "MARK_CONCURRENCY не число",
			env: map[string]string{
				"TOKEN": "t", "DATABASE_URL": "postgres://db", "MARK_CONCURRENCY": "много",
			},
			wantErr: true,
		},
		{
			name: "PORT числом — дополняется двоеточием",
			env: map[string]string{
				"TOKEN": "t", "DATABASE_URL": "postgres://db", "PORT": "9000",
			},
			want: Config{
				Addr: ":9000", Token: "t", DSN: "postgres://db",
				Concurrency: 5, LMSBaseURL: "https://lms.uni-dubna.ru",
			},
		},
		{
			name: "PORT с пробелами и адресом хоста — проходит как есть",
			env: map[string]string{
				"TOKEN": "t", "DATABASE_URL": "postgres://db", "PORT": " 127.0.0.1:8081 ",
			},
			want: Config{
				Addr: "127.0.0.1:8081", Token: "t", DSN: "postgres://db",
				Concurrency: 5, LMSBaseURL: "https://lms.uni-dubna.ru",
			},
		},
		{
			name: "LMS_BASE_URL переопределён",
			env: map[string]string{
				"TOKEN": "t", "DATABASE_URL": "postgres://db", "LMS_BASE_URL": "http://localhost:8080",
			},
			want: Config{
				Addr: ":8080", Token: "t", DSN: "postgres://db",
				Concurrency: 5, LMSBaseURL: "http://localhost:8080",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Все переменные конфига задаём явно: пустое значение в tt.env
			// означает «не задана», чтобы окружение не влияло на тест.
			for _, key := range []string{"PORT", "TOKEN", "DATABASE_URL", "MARK_CONCURRENCY", "LMS_BASE_URL"} {
				t.Setenv(key, tt.env[key])
			}

			got, err := LoadConfig()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("LoadConfig() = %+v, ожидается ошибка", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfig() неожиданная ошибка: %v", err)
			}
			if got != tt.want {
				t.Errorf("LoadConfig() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestAddrFromPort(t *testing.T) {
	tests := []struct {
		name string
		port string
		want string
	}{
		{name: "пусто — адрес по умолчанию", port: "", want: ":8080"},
		{name: "только порт", port: "8080", want: ":8080"},
		{name: "пробелы вокруг порта", port: " 8080\t", want: ":8080"},
		{name: "уже с двоеточием", port: ":8080", want: ":8080"},
		{name: "хост с портом", port: "127.0.0.1:9000", want: "127.0.0.1:9000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := addrFromPort(tt.port); got != tt.want {
				t.Errorf("addrFromPort(%q) = %q, want %q", tt.port, got, tt.want)
			}
		})
	}
}

func TestEnvDefault(t *testing.T) {
	t.Run("переменная задана — возвращается она", func(t *testing.T) {
		t.Setenv("FLMS_TEST_ENV_KEY", "значение")
		if got := envDefault("FLMS_TEST_ENV_KEY", "по умолчанию"); got != "значение" {
			t.Errorf("envDefault() = %q, want %q", got, "значение")
		}
	})
	t.Run("переменная пуста — возвращается умолчание", func(t *testing.T) {
		t.Setenv("FLMS_TEST_ENV_KEY", "")
		if got := envDefault("FLMS_TEST_ENV_KEY", "по умолчанию"); got != "по умолчанию" {
			t.Errorf("envDefault() = %q, want %q", got, "по умолчанию")
		}
	})
}
