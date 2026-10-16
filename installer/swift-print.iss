; Swift Print installer (Inno Setup 7.1.0)
;
; Build from the repository root after building "dist\Swift Print.exe":
;   iscc installer\swift-print.iss
;   iscc /DAppVersion=1.2.3 installer\swift-print.iss

#ifndef AppVersion
  #define AppVersion "1.0.0"
#endif

#define AppName "Swift Print"
#define AppExeName "Swift Print.exe"
#define AppPublisher "Hemant Makhija"
#define AppURL "https://github.com/makhijahemant/swift-print"

[Setup]
; Never change AppId: upgrades and uninstall depend on it.
AppId={{26B23FCF-AE4F-43B5-9A60-D18B12BB54E8}
AppName={#AppName}
AppVersion={#AppVersion}
AppVerName={#AppName} {#AppVersion}
AppPublisher={#AppPublisher}
AppPublisherURL={#AppURL}
AppSupportURL={#AppURL}
AppUpdatesURL={#AppURL}
VersionInfoVersion={#AppVersion}
VersionInfoDescription={#AppName} Setup
VersionInfoCopyright=Copyright (c) {#AppPublisher}. MIT License.

; Per-user install: no UAC prompt, and the app can write config.json beside itself.
PrivilegesRequired=lowest
DefaultDirName={autopf}\{#AppName}
DisableDirPage=yes
DisableProgramGroupPage=yes
DefaultGroupName={#AppName}

ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0

SetupIconFile=..\cmd\swift-print\swiftprint.ico
UninstallDisplayIcon={app}\{#AppExeName}
UninstallDisplayName={#AppName}
LicenseFile=..\LICENSE
WizardStyle=modern
Compression=lzma2
SolidCompression=yes
CloseApplications=force

OutputDir=..\dist
OutputBaseFilename={#AppName} Setup

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Files]
Source: "..\dist\{#AppExeName}"; DestDir: "{app}"; Flags: ignoreversion
; Keep the user's port and token on upgrade.
Source: "..\config.example.json"; DestDir: "{app}"; DestName: "config.json"; Flags: onlyifdoesntexist

[Icons]
Name: "{group}\{#AppName}"; Filename: "{app}\{#AppExeName}"
Name: "{group}\Uninstall {#AppName}"; Filename: "{uninstallexe}"

[Registry]
Root: HKCU; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; ValueType: string; ValueName: "SwiftPrint"; ValueData: """{app}\{#AppExeName}"""; Flags: uninsdeletevalue

[Run]
Filename: "{app}\{#AppExeName}"; Description: "{cm:LaunchProgram,{#AppName}}"; Flags: nowait postinstall skipifsilent

[UninstallRun]
Filename: "{sys}\taskkill.exe"; Parameters: "/F /IM ""{#AppExeName}"""; Flags: runhidden waituntilterminated; RunOnceId: "StopSwiftPrint"

[UninstallDelete]
Type: files; Name: "{app}\config.json"
Type: dirifempty; Name: "{app}"

[Code]
procedure StopSwiftPrint;
var
  ResultCode: Integer;
begin
  Exec(ExpandConstant('{sys}\taskkill.exe'), '/F /IM "{#AppExeName}"', '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
begin
  StopSwiftPrint;
  Result := '';
end;
