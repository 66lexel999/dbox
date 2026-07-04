; D BOX — Inno Setup installer script
; Build:  ISCC.exe build\installer.iss   (run from the repo root)
; Output: dist\DBox-Setup-<version>.exe
;
; Per-user install (no UAC prompt) into %LOCALAPPDATA%\Programs\D BOX; an
; elevated user can still force an all-users install with /ALLUSERS.
; The optional "Video download tools" component bundles yt-dlp + aria2c so
; YouTube/Instagram downloads work out of the box. ffmpeg (227 MB) is NOT
; bundled — D BOX also finds tools in <Downloads>\flowerX\Programs, and
; yt-dlp still handles progressive formats without it.

#define MyAppName "D BOX"
; Version may be passed on the command line: ISCC /DMyAppVersion=1.2.0 ...
; (the release script does this). Falls back to 1.0.0 for a bare compile.
#ifndef MyAppVersion
  #define MyAppVersion "1.0.0"
#endif
#define MyAppExeName "DBox.exe"
#define MyAppPublisher "D BOX"
; Folder holding yt-dlp.exe / aria2c.exe to bundle. Overridable:
;   ISCC /DToolsDir=C:\path\to\tools build\installer.iss
; Defaults to a repo-relative "tools" folder (release.sh + CI stage the tools
; there). Missing files are skipped (skipifsourcedoesntexist), so a bare compile
; without the tools still produces a working app-only installer.
#ifndef ToolsDir
  #define ToolsDir "tools"
#endif

[Setup]
AppId={{7D8E3B4A-5C21-4A9F-9B67-DB01DB01DB01}
AppName={#MyAppName}
AppVersion={#MyAppVersion}
AppPublisher={#MyAppPublisher}
DefaultDirName={autopf}\{#MyAppName}
DefaultGroupName={#MyAppName}
DisableProgramGroupPage=yes
PrivilegesRequired=lowest
PrivilegesRequiredOverridesAllowed=commandline
OutputDir=..\dist
OutputBaseFilename=DBox-Setup-{#MyAppVersion}
SetupIconFile=..\build\appicon.ico
UninstallDisplayIcon={app}\{#MyAppExeName}
Compression=lzma2/max
SolidCompression=yes
WizardStyle=modern
; CloseApplications is OFF on purpose: the Restart Manager would otherwise try
; to shut a D BOX running from ANOTHER path (a portable copy). The [Code] hook
; below closes only the instance in THIS install dir, precisely.
CloseApplications=no
ArchitecturesInstallIn64BitMode=x64compatible

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Components]
Name: "app";   Description: "D BOX download manager";                        Types: full compact custom; Flags: fixed
Name: "tools"; Description: "Video download tools (yt-dlp + aria2c, 24 MB)"; Types: full

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"

[Files]
Source: "..\bin\{#MyAppExeName}"; DestDir: "{app}"; Flags: ignoreversion; Components: app
Source: "{#ToolsDir}\yt-dlp.exe"; DestDir: "{app}"; Flags: ignoreversion skipifsourcedoesntexist; Components: tools
Source: "{#ToolsDir}\aria2c.exe"; DestDir: "{app}"; Flags: ignoreversion skipifsourcedoesntexist; Components: tools

[Icons]
Name: "{autoprograms}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"
Name: "{autodesktop}\{#MyAppName}";  Filename: "{app}\{#MyAppExeName}"; Tasks: desktopicon

[Run]
Filename: "{app}\{#MyAppExeName}"; Description: "{cm:LaunchProgram,{#MyAppName}}"; Flags: nowait postinstall skipifsilent

[Code]
// Stop ONLY the instance running from THIS install directory before an
// upgrade/uninstall — a plain "taskkill /IM DBox.exe" would also kill an
// unrelated portable copy running from somewhere else.
procedure KillAppFromInstallDir();
var
  ResultCode: Integer;
  Cmd: String;
begin
  if not FileExists(ExpandConstant('{app}\{#MyAppExeName}')) then Exit;
  Cmd := '-NoProfile -Command "Get-Process -Name ''DBox'' -ErrorAction SilentlyContinue | ' +
         'Where-Object { $_.Path -eq ''' + ExpandConstant('{app}\{#MyAppExeName}') + ''' } | ' +
         'Stop-Process -Force -ErrorAction SilentlyContinue"';
  Exec('powershell.exe', Cmd, '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
begin
  KillAppFromInstallDir();
  Result := '';
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
begin
  if CurUninstallStep = usUninstall then
    KillAppFromInstallDir();
end;
