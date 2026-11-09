package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// Статусы результата отметки — совпадают с openapi.yaml (MarkResultEvent.status).
const (
	MarkStatusSuccess = "success" // отметка сохранена
	MarkStatusAlready = "already" // уже была отмечена (считается успехом)
	MarkStatusError   = "error"   // сбой этого студента, остальные продолжают
)

// MarkResult — полезная нагрузка одного SSE-события result.
type MarkResult struct {
	Login   string `json:"login"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

// lmsClient — HTTP-клиент Moodle для одного студента.
// У каждого клиента свой cookie jar, чтобы сессии студентов не смешивались.
type lmsClient struct {
	baseURL string
	http    *http.Client
}

// Таймаут на один HTTP-запрос к Moodle.
const lmsRequestTimeout = 15 * time.Second

// browserUA — десктопный Chrome, как в прежнем MoodleClient.
const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

func newLMSClient(baseURL string) *lmsClient {
	jar, err := cookiejar.New(nil)
	if err != nil {
		// cookiejar.New(nil) не возвращает ошибок при nil-опциях, но на всякий случай.
		panic(err)
	}
	return &lmsClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{
			Timeout: lmsRequestTimeout,
			Jar:     jar,
		},
	}
}

// logintoken обычно идёт name=... value=..., но на всякий случай ловим и обратный порядок.
var (
	logintokenRe      = regexp.MustCompile(`name=["']logintoken["'][^>]*value=["']([^"']+)["']`)
	logintokenReAlt   = regexp.MustCompile(`value=["']([^"']+)["'][^>]*name=["']logintoken["']`)
	sesskeyRe         = regexp.MustCompile(`sesskey=([a-zA-Z0-9]+)`)
	attendanceTitleRe = regexp.MustCompile(`<title>[^<]*(Вход на сайт|Login)[^<]*</title>`)
)

// findLogintoken извлекает скрытое поле logintoken из HTML страницы входа.
func findLogintoken(page string) string {
	if m := logintokenRe.FindStringSubmatch(page); m != nil {
		return m[1]
	}
	if m := logintokenReAlt.FindStringSubmatch(page); m != nil {
		return m[1]
	}
	return ""
}

// get выполняет GET с браузерным User-Agent и возвращает тело ответа.
func (c *lmsClient) get(ctx context.Context, urlStr string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, lmsRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", browserUA)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := readBody(resp)
	if err != nil {
		return "", err
	}
	return body, nil
}

// postForm отправляет urlencoded-форму с браузерными заголовками.
func (c *lmsClient) postForm(ctx context.Context, urlStr string, form url.Values, referer string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, lmsRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, urlStr,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", referer)
	req.Header.Set("Origin", c.baseURL)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := readBody(resp)
	if err != nil {
		return "", err
	}
	return body, nil
}

// readBody читает тело ответа с ограничением размера (защита от внезапно огромных страниц).
func readBody(resp *http.Response) (string, error) {
	const maxBodySize = 5 << 20 // 5 МБ
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// login выполняет вход в Moodle: GET страницы входа → logintoken → POST формы.
func (c *lmsClient) login(ctx context.Context, login, password string) error {
	page, err := c.get(ctx, c.baseURL+"/login/index.php")
	if err != nil {
		return fmt.Errorf("не удалось открыть страницу входа: %w", err)
	}
	token := findLogintoken(page)
	if token == "" {
		return fmt.Errorf("на странице входа не найден logintoken")
	}

	form := url.Values{
		"username":         {login},
		"password":         {password},
		"rememberusername": {"0"},
		"logintoken":       {token},
	}
	loginURL := c.baseURL + "/login/index.php"
	out, err := c.postForm(ctx, loginURL, form, loginURL)
	if err != nil {
		return fmt.Errorf("ошибка отправки формы входа: %w", err)
	}

	// Если после POST снова показывается форма входа с logintoken или сообщение
	// об ошибке — Moodle нас не пустил.
	if strings.Contains(out, "loginerrormessage") || findLogintoken(out) != "" {
		return fmt.Errorf("неверный логин или пароль")
	}
	return nil
}

// Ключевые слова для поиска статуса «присутствовал» — те же, что были в JS-инъекции.
var presentKeywords = []string{
	"present", "присутств", "был", "была", "были", "явился", "явилась", "уважительн",
}

// pickStatusValue возвращает value радиокнопки статуса «присутствовал»:
// ищем кнопку, чья подпись содержит одно из ключевых слов; иначе — первую.
// Хардкод status=1 из старого MoodleClient не используется: ID статуса не обязан быть 1.
func pickStatusValue(page string) string {
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		return ""
	}
	var first, chosen string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "input" {
			var name, typ, value string
			for _, a := range n.Attr {
				switch a.Key {
				case "name":
					name = a.Val
				case "type":
					typ = a.Val
				case "value":
					value = a.Val
				}
			}
			if typ == "radio" && name == "status" {
				label := strings.ToLower(nodeText(n.Parent))
				if first == "" {
					first = value
				}
				if chosen == "" {
					for _, kw := range presentKeywords {
						if strings.Contains(label, kw) {
							chosen = value
							break
						}
					}
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	if chosen != "" {
		return chosen
	}
	return first
}

// nodeText собирает весь текст внутри узла (для подписей радиокнопок).
func nodeText(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
		}
		for child := x.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(n)
	return b.String()
}

// MarkAttendance выполняет полный сценарий: логин → страница отметки → POST формы.
// qrpass — пароль из QR (может быть пустым).
func (c *lmsClient) MarkAttendance(ctx context.Context, login, password, sessid, qrpass string) MarkResult {
	fail := func(format string, args ...any) MarkResult {
		return MarkResult{Login: login, Status: MarkStatusError, Message: fmt.Sprintf(format, args...)}
	}

	// 1. Логин.
	if err := c.login(ctx, login, password); err != nil {
		return fail("%v", err)
	}

	// 2. Страница отметки по ссылке из QR.
	pageURL := fmt.Sprintf("%s/mod/attendance/attendance.php?sessid=%s", c.baseURL, url.QueryEscape(sessid))
	if qrpass != "" {
		pageURL += "&qrpass=" + url.QueryEscape(qrpass)
	}
	page, err := c.get(ctx, pageURL)
	if err != nil {
		return fail("не удалось открыть страницу отметки: %v", err)
	}
	// Сессия слетела — один повторный логин и повтор страницы.
	if attendanceTitleRe.MatchString(page) {
		if err := c.login(ctx, login, password); err != nil {
			return fail("сессия слетела, повторный вход не удался: %v", err)
		}
		page, err = c.get(ctx, pageURL)
		if err != nil {
			return fail("не удалось открыть страницу отметки: %v", err)
		}
	}

	// 3. «Уже отмечена» — тоже успех.
	lower := strings.ToLower(page)
	if strings.Contains(lower, "уже была отмечена") || strings.Contains(lower, "already been marked") {
		return MarkResult{Login: login, Status: MarkStatusAlready, Message: "Посещаемость уже была отмечена"}
	}

	// 4. sesskey из HTML.
	m := sesskeyRe.FindStringSubmatch(page)
	if m == nil {
		return fail("не найден sesskey (возможно, занятие закрыто)")
	}
	sesskey := m[1]

	// 5. Статус «присутствовал» из радиокнопок.
	status := pickStatusValue(page)
	if status == "" {
		return fail("на странице отметки нет радиокнопок статуса")
	}

	// 6. POST формы отметки.
	form := url.Values{
		"sessid":       {sessid},
		"sesskey":      {sesskey},
		"status":       {status},
		"submitbutton": {"Save"},
	}
	if qrpass != "" {
		form.Set("studentpassword", qrpass)
		form.Set("qrpass", qrpass)
	}
	markURL := fmt.Sprintf("%s/mod/attendance/attendance.php?sessid=%s&mark=1",
		c.baseURL, url.QueryEscape(sessid))
	out, err := c.postForm(ctx, markURL, form, pageURL)
	if err != nil {
		return fail("ошибка отправки формы отметки: %v", err)
	}

	// 7. Разбор результата по HTML ответа.
	outLower := strings.ToLower(out)
	switch {
	case strings.Contains(outLower, "saved") || strings.Contains(outLower, "сохранено") || strings.Contains(outLower, "успешно"):
		return MarkResult{Login: login, Status: MarkStatusSuccess, Message: "Отмечено"}
	case strings.Contains(outLower, "already") || strings.Contains(outLower, "уже отмечен"):
		return MarkResult{Login: login, Status: MarkStatusAlready, Message: "Посещаемость уже была отмечена"}
	default:
		return fail("ответ получен, но отметка не подтвердилась")
	}
}