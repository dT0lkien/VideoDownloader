; Установщик «Скачать видео». Ставит программу в профиль пользователя:
; права администратора не нужны, вопросов не задаёт.
; Собирается через build.sh.
Unicode true
SetCompressor /SOLID lzma
!include "MUI2.nsh"

!define NAME "Скачать видео"
!define KEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\VideoDownloader"

Name "${NAME}"
OutFile "dist/VideoDownloader-Setup.exe" ; латиницей: GitHub портит кириллицу в именах файлов релиза
InstallDir "$LOCALAPPDATA\Programs\VideoDownloader"
RequestExecutionLevel user
ShowInstDetails nevershow
ShowUninstDetails nevershow

!define MUI_FINISHPAGE_TITLE "Готово!"
!define MUI_FINISHPAGE_TEXT "Программа установлена.$\r$\n$\r$\nНа рабочем столе появился значок «${NAME}» — запускайте её оттуда."
!define MUI_FINISHPAGE_RUN "$INSTDIR\VideoDownloader.exe"
!define MUI_FINISHPAGE_RUN_TEXT "Открыть «${NAME}» сейчас"
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "Russian"

!macro CloseApp
  ; Программа открыта (например, ставим поверх старой версии) — закрываем, иначе файлы заняты.
  nsExec::Exec 'taskkill /F /T /IM VideoDownloader.exe'
  Pop $0
  Sleep 500
!macroend

Section
  !insertmacro CloseApp
  SetOutPath "$INSTDIR"
  File /r "dist/app/*.*"
  WriteUninstaller "$INSTDIR\uninstall.exe"
  CreateShortcut "$DESKTOP\${NAME}.lnk" "$INSTDIR\VideoDownloader.exe"
  CreateShortcut "$SMPROGRAMS\${NAME}.lnk" "$INSTDIR\VideoDownloader.exe"
  WriteRegStr HKCU "${KEY}" "DisplayName" "${NAME}"
  WriteRegStr HKCU "${KEY}" "DisplayIcon" "$INSTDIR\VideoDownloader.exe"
  WriteRegStr HKCU "${KEY}" "UninstallString" '"$INSTDIR\uninstall.exe"'
  WriteRegDWORD HKCU "${KEY}" "NoModify" 1
  WriteRegDWORD HKCU "${KEY}" "NoRepair" 1
SectionEnd

Section "Uninstall"
  !insertmacro CloseApp
  Delete "$DESKTOP\${NAME}.lnk"
  Delete "$SMPROGRAMS\${NAME}.lnk"
  RMDir /r "$INSTDIR"
  RMDir /r "$LOCALAPPDATA\VideoDownloader" ; журнал и недокачанное; скачанные видео не трогаем
  DeleteRegKey HKCU "${KEY}"
SectionEnd
