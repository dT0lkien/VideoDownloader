package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Весь цикл скачивания с подставным yt-dlp: успех, ошибка с повтором, отмена.
func TestDownload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("подставной yt-dlp — это sh-скрипт")
	}
	run := func(script string, cancelAfter time.Duration) *app {
		dir := t.TempDir()
		a := &app{dir: dir, data: dir, def: filepath.Join(dir, "видео"), tmp: filepath.Join(dir, "tmp")}
		os.WriteFile(filepath.Join(dir, "yt-dlp.exe"), []byte("#!/bin/sh\n"+script), 0o755)
		ctx, cancel := context.WithCancel(context.Background())
		a.cancel, a.st = cancel, state{Phase: "prep"}
		if cancelAfter > 0 {
			time.AfterFunc(cancelAfter, a.stop)
		}
		a.download(ctx, "https://example.com/v", 720)
		return a
	}

	// Скрипт проверяет, что выбранное качество дошло до yt-dlp.
	a := run(`case "$*" in *res:720,*) ;; *) echo "ERROR: не то качество: $*"; exit 1;; esac
echo "PRG|downloading|1|2|1|Кино"; echo "DONE|/tmp/Кино.mp4"`, 0)
	if a.st.Phase != "done" || a.st.Title != "Кино" || a.file != "/tmp/Кино.mp4" || a.busy() {
		t.Errorf("успех: %+v file=%q err=%q", a.st, a.file, a.errText)
	}

	// Первая попытка падает, после «обновления» вторая проходит.
	a = run(`[ "$3" = nightly ] && touch "$0.updated" && exit 0
[ -f "$0.updated" ] && echo "DONE|/tmp/x.mp4" && exit 0
echo "ERROR: nsig extraction failed" >&2; exit 1`, 0)
	if a.st.Phase != "done" {
		t.Errorf("повтор после обновления: %+v", a.st)
	}

	a = run(`echo "ERROR: [youtube] x: Private video" >&2; exit 1`, 0)
	if a.st.Phase != "err" || a.st.Error == "" || a.st.Details == "" {
		t.Errorf("ошибка: %+v", a.st)
	}

	start := time.Now()
	a = run(`echo "PRG|downloading|1|2|1|Кино"; sleep 30`, 300*time.Millisecond)
	if a.st.Phase != "idle" || a.busy() || time.Since(start) > 10*time.Second {
		t.Errorf("отмена: %+v за %v", a.st, time.Since(start))
	}
}

// Предпросмотр с подставным yt-dlp: название, обложка, длительность, варианты качества.
func TestInfo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("подставной yt-dlp — это sh-скрипт")
	}
	look := func(script string) *preview {
		dir := t.TempDir()
		a := &app{dir: dir, set: settings{Quality: 720}}
		os.WriteFile(filepath.Join(dir, "yt-dlp.exe"), []byte("#!/bin/sh\n"+script), 0o755)
		return a.info(context.Background(), "https://example.com/v")
	}
	p := look(`echo 'WARNING: что-то не так'
echo 'INFO|{"title": "Кино", "thumbnail": "https://example.com/t.jpg", "duration": 754}'
echo 'FMT|[{"vcodec": "none"}, {"vcodec": "none", "acodec": "mp4a.40.2", "filesize": 10485760},
 {"width": 1920, "height": 1080, "vcodec": "avc1.640028", "acodec": "none", "filesize": 209715200},
 {"width": 1280, "height": 720, "vcodec": "avc1.4d401f", "acodec": "none", "filesize": 94371840}]' | tr -d '\n'; echo`)
	if p == nil || p.Title != "Кино" || p.Thumb != "https://example.com/t.jpg" || p.Duration != "12:34" || p.Error != "" {
		t.Fatalf("предпросмотр: %+v", p)
	}
	want := []option{{1080, "Высокое (1080p)", "примерно 210 МБ"}, {720, "Хорошее (720p)", "примерно 100 МБ"}}
	if len(p.Options) != 2 || p.Options[0] != want[0] || p.Options[1] != want[1] || p.Pick != 1 {
		t.Errorf("варианты: %+v pick=%d", p.Options, p.Pick)
	}

	if p = look(`echo "ERROR: [youtube] x: Private video" >&2; exit 1`); p == nil || p.Error == "" {
		t.Errorf("закрытое видео: %+v", p)
	}
	if p = look(`echo "ERROR: nsig extraction failed" >&2; exit 1`); p != nil {
		t.Errorf("поломка yt-dlp не должна пугать заранее: %+v", p)
	}
}

func TestOptions(t *testing.T) {
	// Вертикальное видео 1080×1920 — это 1080p, а не 1920p; из двух кодеков берём h264.
	got := options([]format{
		{Width: 1080, Height: 1920, Vcodec: "vp9", Acodec: "none", Filesize: 900 << 20},
		{Width: 1080, Height: 1920, Vcodec: "avc1", Acodec: "none", Filesize: 300 << 20},
		{Width: 1080, Height: 1920, Vcodec: "avc1", Acodec: "none", Tbr: 9000}, // тот же формат потоком: размера нет, битрейт завышен
		{Width: 2160, Height: 3840, Vcodec: "vp9", Acodec: "none", Filesize: 4 << 30},
		{Vcodec: "none", Acodec: "opus", Filesize: 50 << 20},
		{Vcodec: "none", Acodec: "mp4a.40.2", Filesize: 20 << 20},
	}, 600)
	if len(got) != 1 || got[0] != (option{1080, "Высокое (1080p)", "примерно 320 МБ"}) {
		t.Errorf("вертикальное: %+v", got)
	}
	// Сайт без размеров файлов и без отдельного звука: считаем по битрейту (кбит/с × секунды).
	got = options([]format{{Height: 480, Tbr: 800}, {Height: 240, Tbr: 300}, {Height: 404, Tbr: 600}}, 1000)
	if len(got) != 2 || got[0] != (option{480, "Среднее (480p)", "примерно 95 МБ"}) || got[1] != (option{240, "Низкое (240p)", "примерно 36 МБ"}) {
		t.Errorf("по битрейту: %+v", got)
	}
	// Сайт вообще ничего не сообщил о размерах — один вариант без приписки.
	got = options([]format{{Vcodec: "h264"}}, 0)
	if len(got) != 1 || got[0] != (option{0, "Обычное качество", ""}) {
		t.Errorf("без размеров: %+v", got)
	}
	if got = options(nil, 60); len(got) != 0 {
		t.Errorf("нет форматов: %+v", got)
	}
}

func TestClock(t *testing.T) {
	for sec, want := range map[float64]string{0: "", 19: "0:19", 754.6: "12:34", 3723: "1:02:03"} {
		if got := clock(sec); got != want {
			t.Errorf("clock(%v) = %q, нужно %q", sec, got, want)
		}
	}
}

func TestLine(t *testing.T) {
	a := &app{st: state{Phase: "prep"}}
	for _, s := range []string{
		"[youtube] Extracting URL",
		"PRG|downloading|1048576|4194304|12|Котики",
		"PRG|downloading|2097152|4194304|7|Котики",
		"PRG|downloading|1048576|NA|NA|Котики", // оценка размера пропала — шкала не откатывается
	} {
		a.line(s)
	}
	if a.st.Phase != "down" || a.st.Title != "Котики" || a.st.Percent != 50 {
		t.Fatalf("прогресс: %+v", a.st)
	}
	a.line("PRG|finished|4194304|4194304|NA|Котики")
	a.line("PRG|downloading|100|1000|1|Котики") // звуковая дорожка
	if a.st.Phase != "fin" || a.st.Percent != 100 || a.st.Info != "Скачиваю звук… 10%" {
		t.Fatalf("после картинки: %+v", a.st)
	}
	a.line("PRG|downloading|1|10|1|Другое | видео") // следующее видео из плейлиста, в названии есть «|»
	if a.st.Phase != "down" || a.st.Title != "Другое | видео" || a.st.Percent != 10 {
		t.Fatalf("следующее видео: %+v", a.st)
	}
	a.line(`DONE|C:\Users\Мама\Videos\Котики [abc].mp4`)
	a.line("ERROR: [youtube] abc: Private video")
	if a.file != `C:\Users\Мама\Videos\Котики [abc].mp4` || a.errText == "" {
		t.Fatalf("file=%q err=%q", a.file, a.errText)
	}
}

func TestProgressText(t *testing.T) {
	for want, got := range map[string]string{
		"45 МБ из 1,5 ГБ · осталось около 2 мин": progressText(45<<20, 1.5*(1<<30), 120),
		"Скачано 3 МБ": progressText(3<<20, 0, 0),
		"1 МБ из 2 МБ · осталось меньше минуты":   progressText(1<<20, 2<<20, 5),
		"1 МБ из 2 МБ · осталось около 1 ч 1 мин": progressText(1<<20, 2<<20, 3700),
	} {
		if got != want {
			t.Errorf("получилось %q, нужно %q", got, want)
		}
	}
}

func TestExplain(t *testing.T) {
	for details, final := range map[string]bool{
		"ERROR: Unsupported URL: https://example.com/":                                           true,
		"ERROR: [youtube] x: Private video. Sign in if you've been granted access to this video": true,
		"ERROR: [youtube] x: Video unavailable. This video has been removed by the uploader":     true,
		"ERROR: unable to write data: [Errno 28] No space left on device":                        true,
		"ERROR: folder unavailable: mkdir E:\\Видео: The device is not ready.":                   true,
		"ERROR: [youtube] x: Sign in to confirm you’re not a bot":                                false,
		"ERROR: Unable to download webpage: <urlopen error [Errno 11001] getaddrinfo failed>":    false,
		"ERROR: [youtube] x: nsig extraction failed":                                             false,
	} {
		if _, got := explain(details); got != final {
			t.Errorf("%q: final=%v, нужно %v", details, got, final)
		}
	}
}

func TestFindURL(t *testing.T) {
	for in, want := range map[string]string{
		"Смотри: https://youtu.be/abc?t=5 — класс": "https://youtu.be/abc?t=5",
		"  https://ok.ru/video/123\n":              "https://ok.ru/video/123",
		"просто текст":                             "",
		"--exec calc":                              "",
	} {
		if got := findURL(in); got != want {
			t.Errorf("findURL(%q) = %q, нужно %q", in, got, want)
		}
	}
}
