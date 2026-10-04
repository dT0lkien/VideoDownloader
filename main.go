// «Скачать видео» — оболочка над yt-dlp для тех, кто не дружит с компьютером.
// Интерфейс — страница ui.html, программа отдаёт её сама себе по 127.0.0.1;
// на Windows страница показывается в собственном окне (os_windows.go).
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const appTitle = "Скачать видео"

//go:embed ui.html
var ui []byte

// state — всё, что видит страница.
type state struct {
	Phase   string  `json:"phase"`   // idle | prep | down | fin | done | err
	Title   string  `json:"title"`   // название видео
	Percent float64 `json:"percent"` // -1 — размер неизвестен
	Info    string  `json:"info"`    // «45 МБ из 120 МБ · осталось около 2 мин»
	Error   string  `json:"error"`   // понятное объяснение
	Details string  `json:"details"` // что сказал yt-dlp — для того, кто будет помогать
}

type app struct {
	dir, dest, tmp string // папка программы, папка с видео, папка для недокачанного

	mu      sync.Mutex
	st      state
	file    string             // последнее скачанное видео
	errText string             // последняя ошибка yt-dlp
	cancel  context.CancelFunc // не nil, пока идёт скачивание

	upd  sync.Mutex   // занят, пока yt-dlp обновляет сам себя
	seen atomic.Int64 // когда страница последний раз спрашивала состояние
}

func main() {
	if !singleInstance() {
		return
	}
	exe, _ := os.Executable()
	cache, _ := os.UserCacheDir()
	data := filepath.Join(cache, "VideoDownloader")
	a := &app{
		dir:  filepath.Dir(exe),
		dest: filepath.Join(videosDir(), "Скачанные видео"),
		tmp:  filepath.Join(data, "tmp"),
		st:   state{Phase: "idle"},
	}
	os.RemoveAll(a.tmp) // недокачанное с прошлого запуска
	os.MkdirAll(a.dest, 0o755)
	os.MkdirAll(data, 0o755)
	logPath := filepath.Join(data, "log.txt")
	if fi, err := os.Stat(logPath); err == nil && fi.Size() > 1<<20 {
		os.Remove(logPath)
	}
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		log.SetOutput(f)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	// Секрет в адресе: чужие сайты и программы не смогут командовать скачиванием.
	token := make([]byte, 16)
	rand.Read(token)
	url := fmt.Sprintf("http://%s/%x/", ln.Addr(), token)
	go http.Serve(ln, a.routes(fmt.Sprintf("/%x", token)))
	go a.update()

	fmt.Println(url)
	if !runWindow(url, filepath.Join(data, "webview")) {
		// WebView2 нет — показываем ту же страницу во вкладке браузера
		// и живём, пока она открыта.
		if runtime.GOOS == "windows" {
			openPath(url)
		}
		a.seen.Store(time.Now().Unix())
		// ponytail: 2 минуты тишины = вкладку закрыли; свёрнутая вкладка может
		// молчать дольше — если это начнёт мешать, перейти на SSE.
		for a.busy() || time.Now().Unix()-a.seen.Load() < 120 {
			time.Sleep(time.Second)
		}
	}
	a.stop()
}

func (a *app) routes(prefix string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(ui)
	})
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, r *http.Request) {
		a.seen.Store(time.Now().Unix())
		a.mu.Lock()
		st := a.st
		a.mu.Unlock()
		json.NewEncoder(w).Encode(st)
	})
	mux.HandleFunc("GET /clipboard", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, findURL(clipboardText()))
	})
	mux.HandleFunc("POST /start", func(w http.ResponseWriter, r *http.Request) {
		url := findURL(r.FormValue("url"))
		if url == "" {
			http.Error(w, "нет ссылки", http.StatusBadRequest)
			return
		}
		a.start(url)
	})
	mux.HandleFunc("POST /cancel", func(w http.ResponseWriter, r *http.Request) { a.stop() })
	mux.HandleFunc("POST /open/{what}", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		file := a.file
		a.mu.Unlock()
		if _, err := os.Stat(file); r.PathValue("what") == "file" && err == nil {
			openPath(file)
		} else {
			openPath(a.dest)
		}
	})
	return http.StripPrefix(prefix, mux)
}

func (a *app) busy() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cancel != nil
}

func (a *app) stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
	}
}

func (a *app) start(url string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel, a.st = cancel, state{Phase: "prep"}
	go a.download(ctx, url)
}

func (a *app) download(ctx context.Context, url string) {
	log.Print("скачиваю ", url)
	ok := false
	for try := 0; ; try++ {
		a.upd.Lock() // ждём, если yt-dlp сейчас обновляется
		a.upd.Unlock()
		if ok = a.ytdlp(ctx, url); ok || ctx.Err() != nil || try > 0 {
			break
		}
		if _, final := explain(a.errText); final {
			break
		}
		// Чаще всего ломается сам yt-dlp (сайты меняются) — обновляем и пробуем ещё раз.
		a.mu.Lock()
		a.st = state{Phase: "prep"}
		a.mu.Unlock()
		a.update()
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	canceled := ctx.Err() != nil
	a.cancel()
	a.cancel = nil
	switch {
	case canceled:
		os.RemoveAll(a.tmp)
		a.st = state{Phase: "idle"}
	case ok:
		a.st = state{Phase: "done", Title: a.st.Title}
	default:
		msg, _ := explain(a.errText)
		a.st = state{Phase: "err", Error: msg, Details: a.errText}
	}
}

// ytdlp запускает одно скачивание и ждёт его конца. Что пошло не так — в a.errText.
func (a *app) ytdlp(ctx context.Context, url string) bool {
	a.mu.Lock()
	a.file, a.errText = "", ""
	a.mu.Unlock()
	exe, err := a.tool()
	if err != nil {
		a.line("ERROR: yt-dlp missing: " + err.Error())
		return false
	}
	cmd := a.command(ctx, exe,
		"--ignore-config", "--no-playlist",
		"-S", "res:1080,vcodec:h264,acodec:aac", "--merge-output-format", "mp4",
		"-P", a.dest, "-P", "temp:"+a.tmp,
		"-o", "%(title).120s [%(id)s].%(ext)s", // обрезаем название: путь в Windows не длиннее 260 знаков
		"--no-mtime", // дата файла = день скачивания, свежее видео сверху
		"--socket-timeout", "30",
		"--encoding", "utf-8", "--color", "never",
		"--newline", "--progress", "--no-simulate",
		"--progress-template", "download:PRG|%(progress.status)s|%(progress.downloaded_bytes)s|%(progress.total_bytes,progress.total_bytes_estimate)s|%(progress.eta)s|%(info.title)s",
		"--print", "after_move:DONE|%(filepath)s",
		"--", url)
	cmd.Cancel = func() error { killTree(cmd); return nil }
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		a.line("ERROR: " + err.Error())
		return false
	}
	track(cmd)
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		a.line(sc.Text())
	}
	io.Copy(io.Discard, out) // если строка не влезла в Scanner — не даём yt-dlp зависнуть на записи
	cmd.Wait()

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file == "" && a.errText == "" {
		a.errText = "yt-dlp ничего не скачал"
	}
	return a.file != ""
}

// line разбирает одну строку вывода yt-dlp.
func (a *app) line(s string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case strings.HasPrefix(s, "PRG|"):
		f := strings.SplitN(s, "|", 6) // PRG|статус|скачано|всего|осталось секунд|название
		if len(f) < 6 {
			return
		}
		if a.st.Phase == "prep" || a.st.Title != f[5] { // началось (следующее) видео
			a.st = state{Phase: "down", Title: f[5], Percent: -1}
		}
		got, _ := strconv.ParseFloat(f[2], 64)
		total, _ := strconv.ParseFloat(f[3], 64)
		eta, _ := strconv.ParseFloat(f[4], 64)
		pct := -1.0
		if total > 0 {
			pct = min(100, got/total*100)
		}
		switch {
		case a.st.Phase == "fin":
			// Картинка уже скачана, следом идёт звук — он маленький, шкалу назад не откатываем.
			if a.st.Info = "Скачиваю звук…"; pct >= 0 && f[1] != "finished" {
				a.st.Info = fmt.Sprintf("Скачиваю звук… %.0f%%", pct)
			}
		case f[1] == "finished":
			a.st.Phase, a.st.Percent, a.st.Info = "fin", 100, ""
		default:
			a.st.Percent = max(a.st.Percent, pct)
			a.st.Info = progressText(got, total, eta)
		}
	case strings.HasPrefix(s, "DONE|"):
		a.file = s[len("DONE|"):]
		log.Print(s)
	case strings.HasPrefix(s, "ERROR:"):
		a.errText = s
		log.Print(s)
	default:
		log.Print(s)
	}
}

func progressText(got, total, eta float64) string {
	s := "Скачано " + size(got)
	if total > 0 {
		s = size(got) + " из " + size(total)
	}
	switch {
	case eta >= 3600:
		s += fmt.Sprintf(" · осталось около %d ч %d мин", int(eta)/3600, int(eta)%3600/60)
	case eta >= 60:
		s += fmt.Sprintf(" · осталось около %.0f мин", eta/60)
	case eta > 0:
		s += " · осталось меньше минуты"
	}
	return s
}

func size(b float64) string {
	if b >= 1<<30 {
		return strings.Replace(fmt.Sprintf("%.1f ГБ", b/(1<<30)), ".", ",", 1)
	}
	return fmt.Sprintf("%.0f МБ", b/(1<<20))
}

var reasons = []struct {
	re    *regexp.Regexp
	msg   string
	final bool // обновление yt-dlp и вторая попытка не помогут
}{
	{regexp.MustCompile(`(?i)unsupported url|not a valid url|no suitable extractor`),
		"По этой ссылке не получилось найти видео. Откройте страницу с видео, скопируйте её адрес целиком и попробуйте ещё раз.", true},
	{regexp.MustCompile(`(?i)not a bot|too many requests|http error 429`),
		"Сайт временно не отдаёт видео — слишком много запросов. Подождите немного и попробуйте ещё раз.", false},
	{regexp.MustCompile(`(?i)private|sign in|log ?in|members|registered users|confirm your age|authenticat`),
		"Это видео закрыто: посмотреть его можно только после входа на сайт. Скачать его не получится.", true},
	{regexp.MustCompile(`(?i)no space left|errno 28|not enough space|disk is full`),
		"На компьютере закончилось место. Удалите ненужные файлы и попробуйте ещё раз.", true},
	{regexp.MustCompile(`(?i)getaddrinfo|name resolution|timed out|urlopen error|connection (reset|refused|aborted)|unreachable|errno 1100[14]|winerror 100`),
		"Не получается связаться с сайтом. Проверьте, работает ли интернет, и попробуйте ещё раз.", false},
	{regexp.MustCompile(`(?i)unavailable|removed|deleted|does not exist|not available|http error 404`),
		"Это видео недоступно — возможно, его удалили или закрыли.", true},
	{regexp.MustCompile(`(?i)yt-dlp missing`),
		"Программе не хватает одной из её частей, а скачать её заново не вышло. Проверьте интернет и попробуйте ещё раз. Если не поможет — установите программу заново.", true},
}

// explain переводит ошибку yt-dlp на человеческий язык.
func explain(details string) (msg string, final bool) {
	for _, r := range reasons {
		if r.re.MatchString(details) {
			return r.msg, r.final
		}
	}
	return "Не получилось скачать это видео. Попробуйте ещё раз немного позже.", false
}

var urlRe = regexp.MustCompile(`https?://[^\s<>"']+`)

// findURL вытаскивает ссылку из текста: в буфере обмена часто лежит «Смотри: https://…».
func findURL(s string) string { return urlRe.FindString(s) }

func (a *app) command(ctx context.Context, exe string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, exe, args...)
	// ffmpeg и deno лежат рядом с программой — yt-dlp найдёт их через PATH.
	cmd.Env = append(os.Environ(), "PATH="+a.dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd.WaitDelay = 5 * time.Second
	hide(cmd)
	return cmd
}

// tool находит yt-dlp; если антивирус его удалил — скачивает заново.
func (a *app) tool() (string, error) {
	p := filepath.Join(a.dir, "yt-dlp.exe")
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	if p, err := exec.LookPath("yt-dlp"); err == nil {
		return p, nil
	}
	if runtime.GOOS != "windows" {
		return "", errors.New("yt-dlp не установлен")
	}
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Get("https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp.exe")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New(resp.Status)
	}
	f, err := os.Create(p + ".tmp")
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	return p, os.Rename(p+".tmp", p)
}

// update просит yt-dlp обновить самого себя. Сайты постоянно меняются,
// без свежего yt-dlp скачивание перестаёт работать; nightly — канал,
// который советуют сами разработчики yt-dlp.
func (a *app) update() {
	a.upd.Lock()
	defer a.upd.Unlock()
	exe, err := a.tool()
	if err != nil {
		log.Print("обновление: ", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	out, err := a.command(ctx, exe, "--ignore-config", "--update-to", "nightly").CombinedOutput()
	log.Printf("обновление: %s %v", out, err)
}
