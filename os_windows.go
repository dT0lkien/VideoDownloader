//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
	"github.com/jchv/go-webview2/webviewloader"
	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
)

func findWindow() uintptr {
	class, _ := windows.UTF16PtrFromString("webview") // класс окна из go-webview2
	title, _ := windows.UTF16PtrFromString(appTitle)
	h, _, _ := user32.NewProc("FindWindowW").Call(uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(title)))
	return h
}

// singleInstance: если программа уже открыта — показываем её окно, второе не открываем.
func singleInstance() bool {
	name, _ := windows.UTF16PtrFromString("VideoDownloader")
	if _, err := windows.CreateMutex(nil, false, name); err != windows.ERROR_ALREADY_EXISTS {
		return true
	}
	// Значок нажали дважды подряд: первая копия ещё не успела показать окно — ждём его.
	for i := 0; i < 20; i++ {
		if h := findWindow(); h != 0 {
			if iconic, _, _ := user32.NewProc("IsIconic").Call(h); iconic != 0 {
				user32.NewProc("ShowWindow").Call(h, windows.SW_RESTORE)
			}
			user32.NewProc("SetForegroundWindow").Call(h)
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
	return true
}

// runWindow показывает страницу в своём окне и возвращается, когда окно закрыли.
// false — WebView2 на компьютере нет.
func runWindow(url, data string) bool {
	if v, err := webviewloader.GetInstalledVersion(); err != nil || v == "" {
		return false
	}
	runtime.LockOSThread() // окно и его цикл сообщений живут в одном потоке

	// Размер окна задаётся в пикселях экрана — учитываем масштаб Windows (125%, 150%…).
	metric := func(i uintptr) int { v, _, _ := user32.NewProc("GetSystemMetrics").Call(i); return int(v) }
	dpi := 96
	if p := user32.NewProc("GetDpiForSystem"); p.Find() == nil {
		v, _, _ := p.Call()
		dpi = int(v)
	}
	width := max(400, min(780*dpi/96, metric(0)))     // SM_CXSCREEN
	height := max(400, min(760*dpi/96, metric(1)-80)) // SM_CYSCREEN, минус панель задач

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		DataPath:  data,
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title: appTitle, Width: uint(width), Height: uint(height), IconId: 1, Center: true,
		},
	})
	if w == nil {
		if h := findWindow(); h != 0 { // библиотека успела создать пустое окно — убираем
			user32.NewProc("DestroyWindow").Call(h)
		}
		return false
	}
	w.SetSize(min(520*dpi/96, width), min(560*dpi/96, height), webview2.HintMin)
	w.Navigate(url)
	w.Run()
	return true
}

func videosDir() string {
	if p, err := windows.KnownFolderPath(windows.FOLDERID_Videos, 0); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Videos")
}

func clipboardText() string {
	runtime.LockOSThread() // открыть и закрыть буфер обмена должен один и тот же поток
	defer runtime.UnlockOSThread()
	if ok, _, _ := user32.NewProc("OpenClipboard").Call(0); ok == 0 {
		return ""
	}
	defer user32.NewProc("CloseClipboard").Call()
	h, _, _ := user32.NewProc("GetClipboardData").Call(13) // CF_UNICODETEXT
	if h == 0 {
		return ""
	}
	p, _, _ := kernel32.NewProc("GlobalLock").Call(h)
	if p == 0 {
		return ""
	}
	defer kernel32.NewProc("GlobalUnlock").Call(h)
	// Память принадлежит Windows, а не Go; unsafe.Add — чтобы go vet не ругался на uintptr.
	return windows.UTF16PtrToString((*uint16)(unsafe.Add(unsafe.Pointer(nil), p)))
}

// openPath открывает папку, файл или ссылку так же, как двойной щелчок в Проводнике.
func openPath(p string) {
	verb, _ := windows.UTF16PtrFromString("open")
	file, _ := windows.UTF16PtrFromString(p)
	windows.ShellExecute(0, verb, file, nil, nil, windows.SW_SHOWNORMAL)
}

// hide: без этого при каждом запуске yt-dlp мелькало бы чёрное окно консоли.
func hide(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}

// job собирает yt-dlp и всё, что он запускает (свою вторую половину, ffmpeg, deno),
// чтобы «Отменить» и закрытие программы останавливали их разом.
var job = func() windows.Handle {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	return h
}()

func track(cmd *exec.Cmd) {
	p, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return
	}
	windows.AssignProcessToJobObject(job, p)
	windows.CloseHandle(p)
}

func killTree(cmd *exec.Cmd) {
	windows.TerminateJobObject(job, 1)
	cmd.Process.Kill()
}
