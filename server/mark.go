package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// MarkRequest — тело POST /api/mark (openapi.yaml).
type MarkRequest struct {
	QRUrl      string  `json:"qrUrl"`
	StudentIDs []int64 `json:"studentIds"`
}

// MarkSummary — полезная нагрузка финального SSE-события done.
type MarkSummary struct {
	Total   int `json:"total"`
	Success int `json:"success"`
	Already int `json:"already"`
	Error   int `json:"error"`
}

// parseQRUrl разбирает сырую ссылку из QR: host должен совпадать с LMS,
// обязателен sessid, пароль берётся из qrpass | password | studentpassword.
func parseQRUrl(raw, lmsBaseURL string) (sessid, qrpass string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("qrUrl не парсится: %v", err)
	}
	base, err := url.Parse(lmsBaseURL)
	if err != nil {
		return "", "", fmt.Errorf("LMS_BASE_URL не парсится: %v", err)
	}
	if u.Host != base.Host {
		return "", "", fmt.Errorf("qrUrl указывает на чужой host: разрешён только %s", base.Host)
	}

	query := u.Query()
	sessid = query.Get("sessid")
	if sessid == "" {
		return "", "", fmt.Errorf("в qrUrl отсутствует sessid")
	}
	// Приоритет имён пароля — как в QrParser и в docs/network-api.md.
	for _, name := range []string{"qrpass", "password", "studentpassword"} {
		if v := query.Get(name); v != "" {
			qrpass = v
			break
		}
	}
	return sessid, qrpass, nil
}

var shadowbanned = map[string]struct{}{
	"manva.25": {},
}

// selectStudents выбирает студентов: явный список id или все с is_selected = true,
func (a *app) selectStudents(ctx context.Context, ids []int64) ([]studentRecord, error) {
	var (
		all []studentRecord
		err error
	)
	if len(ids) > 0 {
		all, err = a.store.StudentsByIDs(ctx, ids)
	} else {
		all, err = a.store.SelectedStudents(ctx)
	}
	if err != nil {
		return nil, err
	}

	filtered := all[:0]
	for _, rec := range all {
		if _, banned := shadowbanned[strings.ToLower(rec.LmsLogin)]; !banned {
			filtered = append(filtered, rec)
		}
	}
	return filtered, nil
}

// handleMark — POST /api/mark: параллельная отметка с SSE-стримом результатов.
func (a *app) handleMark(w http.ResponseWriter, r *http.Request) {
	var req MarkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "неверное тело запроса: "+err.Error())
		return
	}
	sessid, qrpass, err := parseQRUrl(req.QRUrl, a.cfg.LMSBaseURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	students, err := a.selectStudents(r.Context(), req.StudentIDs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ошибка БД: "+err.Error())
		return
	}
	if len(students) == 0 {
		writeError(w, http.StatusBadRequest, "нет студентов для отметки: список id пуст или ни один не выбран")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "SSE не поддерживается этим соединением")
		return
	}

	// SSE-заголовки; X-Accel-Buffering отключает буферизацию nginx.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Пул воркеров: не больше cfg.Concurrency отметок одновременно.
	results := make(chan MarkResult, len(students))
	var wg sync.WaitGroup
	sem := make(chan struct{}, a.cfg.Concurrency)
	for _, rec := range students {
		wg.Add(1)
		go func(rec studentRecord) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// Отдельный клиент с собственным cookie jar на каждого студента.
			// context.Background(), а не r.Context(): обрыв соединения
			// не отменяет начатые отметки (см. docs/network-api.md).
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			client := newLMSClient(a.cfg.LMSBaseURL)
			results <- client.MarkAttendance(ctx, rec.LmsLogin, rec.LmsPassword, sessid, qrpass)
		}(rec)
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	writeEvent := func(event string, payload any) bool {
		data, err := json.Marshal(payload)
		if err != nil {
			return true // ошибка маршлинга невозможна для этих структур; не рвём поток
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return false // клиент отключился
		}
		flusher.Flush()
		return true
	}

	// Heartbeat каждые 15 секунд, чтобы прокси не рвали простаивающее соединение.
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	summary := MarkSummary{Total: len(students)}
	for remaining := len(students); remaining > 0; {
		select {
		case res, ok := <-results:
			if !ok {
				// Канал закрыт раньше времени — завершаем поток, чтобы не зависнуть.
				remaining = 0
				break
			}
			remaining--
			switch res.Status {
			case MarkStatusSuccess:
				summary.Success++
			case MarkStatusAlready:
				summary.Already++
			default:
				summary.Error++
			}
			if !writeEvent("result", res) {
				return
			}
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
	writeEvent("done", summary)
}