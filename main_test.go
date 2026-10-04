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
		a := &app{dir: dir, dest: dir, tmp: filepath.Join(dir, "tmp")}
		os.WriteFile(filepath.Join(dir, "yt-dlp.exe"), []byte("#!/bin/sh\n"+script), 0o755)
		ctx, cancel := context.WithCancel(context.Background())
		a.cancel, a.st = cancel, state{Phase: "prep"}
		if cancelAfter > 0 {
			time.AfterFunc(cancelAfter, a.stop)
		}
		a.download(ctx, "https://example.com/v")
		return a
	}

	a := run(`echo "PRG|downloading|1|2|1|Кино"; echo "DONE|/tmp/Кино.mp4"`, 0)
	if a.st.Phase != "done" || a.st.Title != "Кино" || a.file != "/tmp/Кино.mp4" || a.busy() {
		t.Errorf("успех: %+v file=%q", a.st, a.file)
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
