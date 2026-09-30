; Inno Setup script — build the Windows installer on a Windows machine with
; Inno Setup 6 (https://jrsoftware.org/isinfo.php), e.g.:
;
;   go build -tags nosystray -ldflags "-X main.version=1.0.0" -o build\mpv-shim.exe .
;   iscc /DMyAppVersion=1.0.0 packaging\windows\mpv-shim.iss
;
; Produces mpv-shim-<version>-setup.exe. Silent install: /VERYSILENT
; Remove again from Settings → Apps, or with: uninstall.exe /VERYSILENT

#define MyAppName "Jellyfin MPV Shim"
#define MyAppPublisher "mpv-shim-go contributors"
#define MyAppExeName "mpv-shim.exe"
#ifndef MyAppVersion
  #define MyAppVersion "1.0.0"
#endif

[Setup]
AppId={{6E3A0F6C-9C1E-4C6E-9E4B-3F0C1A2B7D55}
AppName={#MyAppName}
AppVersion={#MyAppVersion}
AppPublisher={#MyAppPublisher}
DefaultDirName={autopf}\{#MyAppName}
DefaultGroupName={#MyAppName}
OutputBaseFilename=mpv-shim-{#MyAppVersion}-setup
Compression=lzma2
SolidCompression=yes
ArchitecturesInstallIn64BitMode=x64compatible
DisableProgramGroupPage=yes
WizardStyle=modern
; mpv is a hard requirement at runtime, but we do not bundle it: point the
; user at https://mpv.io / winget install mpv.sh or choco install mpv.
UninstallDisplayIcon={app}\{#MyAppExeName}

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"; Flags: unchecked

[Files]
Source: "..\..\build\{#MyAppExeName}"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\..\packaging\icon\io.github.ikac.mpv-shim.png"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\..\README.md"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\..\LICENSE"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{group}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"
Name: "{group}\{cm:UninstallProgram,{#MyAppName}}"; Filename: "{uninstallexe}"
Name: "{autodesktop}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"; Tasks: desktopicon

[Run]
Filename: "{app}\{#MyAppExeName}"; Description: "{cm:LaunchProgram,{#MyAppName}}"; Flags: nowait postinstall skipifsilent
