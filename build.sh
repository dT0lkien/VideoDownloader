#!/bin/sh
# Собирает dist/VideoDownloader-Setup.exe на macOS или Linux.
# Нужны: go, makensis (brew install makensis), curl, unzip.
# yt-dlp, ffmpeg и deno для Windows скачиваются один раз в dist/dl;
# чтобы взять свежие — удалите dist/dl.
set -eu
cd "$(dirname "$0")"

fetch() { [ -f "dist/dl/$1" ] || curl -fL --retry 3 -o "dist/dl/$1" "$2"; }
mkdir -p dist/dl
fetch yt-dlp.exe https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp.exe
fetch ffmpeg.zip https://github.com/yt-dlp/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-win64-gpl-shared.zip
fetch deno.zip https://github.com/denoland/deno/releases/latest/download/deno-x86_64-pc-windows-msvc.zip

rm -rf dist/app
mkdir dist/app
cp dist/dl/yt-dlp.exe dist/app/
unzip -qjo dist/dl/ffmpeg.zip '*/bin/ffmpeg.exe' '*/bin/ffprobe.exe' '*/bin/*.dll' -d dist/app
unzip -qo dist/dl/deno.zip deno.exe -d dist/app
# Загрузчик WebView2 кладём рядом: без него библиотека грузит его копию прямо из памяти,
# а такое поведение не любят антивирусы.
cp "$(go list -m -f '{{.Dir}}' github.com/jchv/go-webview2)/webviewloader/sdk/x64/WebView2Loader.dll" dist/app/
chmod u+w dist/app/WebView2Loader.dll

go test ./...
go run github.com/tc-hib/go-winres@v0.3.3 make --arch amd64 # значок и манифест -> rsrc_windows_amd64.syso
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -H windowsgui" -o dist/app/VideoDownloader.exe .
LC_ALL=en_US.UTF-8 makensis -V2 -INPUTCHARSET UTF8 installer.nsi # без UTF-8-локали makensis не читает кириллицу
ls -lh dist/*.exe
