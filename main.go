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

// preview — что известно о видео до скачивания.
type preview struct {
	URL      string   `json:"url"`
	Loading  bool     `json:"loading"`
	Error    string   `json:"error"` // видео точно не скачать — говорим сразу
	Title    string   `json:"title"`
	Thumb    string   `json:"thumb"`    // адрес обложки
	Duration string   `json:"duration"` // «12:34»
	Count    int      `json:"count"`    // сколько видео в подборке, если ссылка на подборку
	Options  []option `json:"options"`
	Pick     int      `json:"pick"` // какой вариант качества предложить
}

type option struct {
	Quality int    `json:"quality"` // меньшая сторона кадра: 1080, 720…; 0 — неизвестна
	Label   string `json:"label"`   // «Высокое (1080p)»
	Size    string `json:"size"`    // «примерно 350 МБ»
}

// settings — то, что программа помнит между запусками (settings.json).
type settings struct {
	Folder  string `json:"folder"` // "" — папка по умолчанию
	Quality int    `json:"quality"`
}

type app struct {
	dir, data, def, tmp string // папка программы, её данных, папка с видео по умолчанию, для недокачанного

	mu       sync.Mutex
	set      settings
	st       state
	file     string             // последнее скачанное видео
	errText  string             // последняя ошибка yt-dlp
	cancel   context.CancelFunc // не nil, пока идёт скачивание
	preview  *preview
	stopLook context.CancelFunc // останавливает незаконченный предпросмотр

	upd     sync.Mutex   // занят, пока yt-dlp обновляет сам себя
	fetch   sync.Mutex   // занят, пока yt-dlp.exe скачивается заново
	picking atomic.Bool  // открыто окно выбора папки
	seen    atomic.Int64 // когда страница последний раз спрашивала состояние
}

// folder — куда сохранять видео. Вызывать под a.mu.
func (a *app) folder() string {
	if a.set.Folder != "" {
		return a.set.Folder
	}
	return a.def
}

// save записывает настройки. Вызывать под a.mu.
func (a *app) save() {
	b, _ := json.Marshal(a.set)
	os.WriteFile(filepath.Join(a.data, "settings.json"), b, 0o644)
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
		data: data,
		def:  filepath.Join(videosDir(), "Скачанные видео"),
		tmp:  filepath.Join(data, "tmp"),
		set:  settings{Quality: 1080},
		st:   state{Phase: "idle"},
	}
	if b, err := os.ReadFile(filepath.Join(data, "settings.json")); err == nil {
		json.Unmarshal(b, &a.set)
	}
	os.RemoveAll(a.tmp) // недокачанное с прошлого запуска
	os.MkdirAll(a.folder(), 0o755)
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
		defer a.mu.Unlock()
		json.NewEncoder(w).Encode(struct {
			state
			Folder  string   `json:"folder"`
			Preview *preview `json:"preview"`
		}{a.st, a.folder(), a.preview})
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
		quality, _ := strconv.Atoi(r.FormValue("quality"))
		a.start(url, quality)
	})
	mux.HandleFunc("POST /look", func(w http.ResponseWriter, r *http.Request) {
		a.look(findURL(r.FormValue("url")))
	})
	mux.HandleFunc("POST /folder", func(w http.ResponseWriter, r *http.Request) {
		if !a.picking.CompareAndSwap(false, true) {
			return
		}
		defer a.picking.Store(false)
		if p := pickFolder(); p != "" {
			a.mu.Lock()
			a.set.Folder = p
			a.save()
			a.mu.Unlock()
		}
	})
	mux.HandleFunc("POST /cancel", func(w http.ResponseWriter, r *http.Request) { a.stop() })
	mux.HandleFunc("POST /open/{what}", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		file, folder := a.file, a.folder()
		a.mu.Unlock()
		if _, err := os.Stat(file); r.PathValue("what") == "file" && err == nil {
			openPath(file)
		} else {
			os.MkdirAll(folder, 0o755)
			openPath(folder)
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

// start начинает скачивание; quality 0 — как в прошлый раз.
func (a *app) start(url string, quality int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		return
	}
	if quality > 0 && quality != a.set.Quality {
		a.set.Quality = quality
		a.save()
	}
	if a.stopLook != nil { // предпросмотр больше не нужен, пусть не мешает скачиванию
		a.stopLook()
		if a.preview != nil && a.preview.Loading {
			a.preview = nil
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel, a.st = cancel, state{Phase: "prep"}
	go a.download(ctx, url, a.set.Quality)
}

// look узнаёт у yt-dlp, что за видео по ссылке; пустая ссылка убирает предпросмотр.
func (a *app) look(url string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		return
	}
	if a.stopLook != nil {
		a.stopLook()
	}
	a.preview, a.stopLook = nil, nil
	if url == "" {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.preview, a.stopLook = &preview{URL: url, Loading: true}, cancel
	go func() {
		defer cancel()
		p := a.info(ctx, url)
		a.mu.Lock()
		defer a.mu.Unlock()
		if ctx.Err() == nil {
			a.preview = p
		}
	}()
}

// info спрашивает у yt-dlp название, обложку, длительность и доступные форматы.
// nil — узнать не вышло, но скачивание ещё может получиться.
func (a *app) info(ctx context.Context, url string) *preview {
	a.upd.Lock() // ждём, если yt-dlp сейчас обновляется
	a.upd.Unlock()
	var meta struct {
		Title, Thumbnail string
		Duration         float64
		Count            int `json:"playlist_count"`
	}
	var formats []format
	var errText string
	a.run(ctx, func(s string) {
		switch {
		case strings.HasPrefix(s, "INFO|"):
			json.Unmarshal([]byte(s[len("INFO|"):]), &meta)
		case strings.HasPrefix(s, "FMT|"):
			json.Unmarshal([]byte(s[len("FMT|"):]), &formats)
		case strings.HasPrefix(s, "ERROR:"):
			errText = s
			log.Print("предпросмотр: ", s)
		}
	},
		"--no-playlist", "--playlist-items", "1", // у подборки смотрим только первое видео
		"--print", "INFO|%(.{title,thumbnail,duration,playlist_count})j",
		"--print", "FMT|%(formats.:.{width,height,vcodec,acodec,filesize,filesize_approx,tbr})j",
		"--", url)

	p := &preview{URL: url}
	if meta.Title == "" && len(formats) == 0 {
		msg, final := explain(errText)
		if !final {
			return nil
		}
		p.Error = msg
		return p
	}
	p.Title, p.Duration, p.Count = meta.Title, clock(meta.Duration), meta.Count
	if strings.HasPrefix(meta.Thumbnail, "http") {
		p.Thumb = meta.Thumbnail
	}
	p.Options = options(formats, meta.Duration)
	a.mu.Lock()
	want := a.set.Quality
	a.mu.Unlock()
	for i, o := range p.Options { // лучший вариант не выше привычного качества, иначе самый скромный
		if p.Pick = i; o.Quality <= want {
			break
		}
	}
	return p
}

// format — один из вариантов видео или звука, которые отдаёт сайт.
type format struct {
	Width, Height  float64
	Vcodec, Acodec string
	Filesize       float64
	FilesizeApprox float64 `json:"filesize_approx"`
	Tbr            float64 // кбит/с
}

// res — меньшая сторона кадра: так yt-dlp меряет качество и у вертикальных видео.
func (f format) res() int {
	w, h := int(f.Width), int(f.Height)
	if w == 0 || h != 0 && h < w {
		return h
	}
	return w
}

func (f format) bytes(duration float64) float64 {
	switch {
	case f.Filesize > 0:
		return f.Filesize
	case f.FilesizeApprox > 0:
		return f.FilesizeApprox
	}
	return f.Tbr * 125 * duration
}

// options прикидывает, какие варианты качества есть у видео и сколько займёт каждый.
// Выбор формата повторяет то, что сделает yt-dlp с «-S res:N,vcodec:h264,acodec:aac»;
// совпадение не гарантировано, поэтому размер — «примерно».
func options(formats []format, duration float64) []option {
	// best: из подходящих форматов — с нужным кодеком; среди них — с известным размером
	// (у YouTube каждый формат есть ещё и в виде потока без размера, yt-dlp такие
	// не выбирает, а битрейт у них завышен втрое); среди равных — покрупнее.
	best := func(fits, preferred func(format) bool) (b format, found bool) {
		score := func(f format) float64 {
			s := f.bytes(duration)
			if f.Filesize > 0 || f.FilesizeApprox > 0 {
				s += 1e15
			}
			if preferred(f) {
				s += 1e16
			}
			return s
		}
		for _, f := range formats {
			if fits(f) && (!found || score(f) > score(b)) {
				b, found = f, true
			}
		}
		return
	}
	video := func(f format) bool { return f.Vcodec != "none" }
	sound, _ := best(
		func(f format) bool { return f.Vcodec == "none" && f.Acodec != "none" && f.Acodec != "" },
		func(f format) bool { return strings.HasPrefix(f.Acodec, "mp4a") || strings.HasPrefix(f.Acodec, "aac") })

	var out []option
	add := func(res int) {
		v, found := best(
			func(f format) bool { return video(f) && f.res() == res },
			func(f format) bool { return strings.HasPrefix(f.Vcodec, "avc") || strings.HasPrefix(f.Vcodec, "h264") })
		if !found {
			return
		}
		b := v.bytes(duration)
		if v.Acodec == "none" { // картинка без звука — звук скачается отдельным файлом
			b += sound.bytes(duration)
		}
		o := option{Quality: res}
		switch {
		case res == 0:
			o.Label = "Обычное качество"
		case res >= 1080:
			o.Label = fmt.Sprintf("Высокое (%dp)", res)
		case res >= 720:
			o.Label = fmt.Sprintf("Хорошее (%dp)", res)
		case res >= 480:
			o.Label = fmt.Sprintf("Среднее (%dp)", res)
		default:
			o.Label = fmt.Sprintf("Низкое (%dp)", res)
		}
		switch {
		case b >= 1<<20:
			o.Size = "примерно " + size(b)
		case b > 0:
			o.Size = "меньше 1 МБ"
		}
		out = append(out, o)
	}
	last := 0
	for _, limit := range []int{1080, 720, 480, 360} { // выше 1080p не предлагаем: огромные файлы, играют не везде
		res := 0
		for _, f := range formats {
			if r := f.res(); video(f) && r <= limit && r > res {
				res = r
			}
		}
		if res > 0 && res != last {
			add(res)
			last = res
		}
	}
	if len(out) == 0 {
		add(0) // сайт не сообщил размеры кадра
	}
	return out
}

// clock: 754 секунды → «12:34».
func clock(seconds float64) string {
	s := int(seconds)
	switch {
	case s <= 0:
		return ""
	case s >= 3600:
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s%3600/60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func (a *app) download(ctx context.Context, url string, quality int) {
	log.Print("скачиваю ", url)
	ok := false
	for try := 0; ; try++ {
		a.upd.Lock() // ждём, если yt-dlp сейчас обновляется
		a.upd.Unlock()
		if ok = a.ytdlp(ctx, url, quality); ok || ctx.Err() != nil || try > 0 {
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
func (a *app) ytdlp(ctx context.Context, url string, quality int) bool {
	a.mu.Lock()
	a.file, a.errText = "", ""
	folder := a.folder()
	a.mu.Unlock()
	if err := writable(folder); err != nil {
		a.line("ERROR: folder unavailable: " + err.Error())
		return false
	}
	a.run(ctx, a.line,
		"--no-playlist",
		"-S", fmt.Sprintf("res:%d,vcodec:h264,acodec:aac", quality), "--merge-output-format", "mp4",
		"-P", folder, "-P", "temp:"+a.tmp,
		"-o", "%(title).120s [%(id)s].%(ext)s", // обрезаем название: путь в Windows не длиннее 260 знаков
		"--no-mtime", // дата файла = день скачивания, свежее видео сверху
		"--newline", "--progress", "--no-simulate",
		"--progress-template", "download:PRG|%(progress.status)s|%(progress.downloaded_bytes)s|%(progress.total_bytes,progress.total_bytes_estimate)s|%(progress.eta)s|%(info.title)s",
		"--print", "after_move:DONE|%(filepath)s",
		"--", url)

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file == "" && a.errText == "" {
		a.errText = "yt-dlp ничего не скачал"
	}
	return a.file != ""
}

// writable проверяет, что в папку можно сохранять: флешку могли вынуть, папка может быть защищена.
func writable(folder string) error {
	if err := os.MkdirAll(folder, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(folder, ".проверка-*")
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(f.Name())
}

// run запускает yt-dlp и отдаёт каждую строку его вывода в line. Отмена ctx
// останавливает его вместе со всем, что он успел запустить (ffmpeg, deno).
func (a *app) run(ctx context.Context, line func(string), args ...string) {
	exe, err := a.tool()
	if err != nil {
		line("ERROR: yt-dlp missing: " + err.Error())
		return
	}
	cmd := a.command(ctx, exe, append([]string{
		"--ignore-config", "--socket-timeout", "30", "--encoding", "utf-8", "--color", "never",
	}, args...)...)
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		line("ERROR: " + err.Error())
		return
	}
	defer context.AfterFunc(ctx, track(cmd))()
	sc := bufio.NewScanner(out)
	sc.Buffer(nil, 4<<20) // список форматов приходит одной длинной строкой
	for sc.Scan() {
		line(sc.Text())
	}
	io.Copy(io.Discard, out) // если строка всё же не влезла — не даём yt-dlp зависнуть на записи
	cmd.Wait()
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
	{regexp.MustCompile(`(?i)folder unavailable`),
		"В выбранную папку сохранить не получается — возможно, вынули флешку или папка защищена. Нажмите «Изменить папку» и выберите другую.", true},
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
	a.fetch.Lock()
	defer a.fetch.Unlock()
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
