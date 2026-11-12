<#
.SYNOPSIS
  Install or upgrade OpenVMS Recorder as a Windows service.
.EXAMPLE
  # From an elevated PowerShell, next to openvms-recorder.exe:
  .\install.ps1 -Binary .\openvms-recorder.exe -Volumes 'D:\OpenVMS\Recordings','E:\OpenVMS\Recordings'
.NOTES
  Never deletes recordings, the database or the configuration.
  FFmpeg is required: winget install --id Gyan.FFmpeg  (or set recording.ffmpeg_path).
#>
param(
  [Parameter(Mandatory)] [string] $Binary,
  [string[]] $Volumes = @(),
  [string] $InstallDir = "$env:ProgramFiles\OpenVMS Recorder",
  [string] $DataDir = "$env:ProgramData\OpenVMS\Recorder"
)
$ErrorActionPreference = 'Stop'
$svc = 'OpenVMSRecorder'
$account = "NT SERVICE\$svc"
$exe = Join-Path $InstallDir 'openvms-recorder.exe'
$config = Join-Path $DataDir 'recorder.yaml'

& $Binary version | Out-Null   # refuse a broken binary before swapping

$existing = Get-Service -Name $svc -ErrorAction SilentlyContinue
if ($existing -and $existing.Status -ne 'Stopped') { Stop-Service $svc; $existing.WaitForStatus('Stopped', '00:00:45') }

New-Item -ItemType Directory -Force -Path $InstallDir, $DataDir | Out-Null
if (Test-Path $exe) { Copy-Item $exe "$exe.prev" -Force }
Copy-Item $Binary $exe -Force

if (-not (Test-Path $config)) {
  $example = Join-Path $PSScriptRoot '..\..\configs\recorder.example.yaml'
  if (Test-Path $example) { Copy-Item $example $config } else { New-Item -ItemType File $config | Out-Null }
  Write-Host "Edit $config (volumes with path_windows, cameras) before starting."
}
foreach ($f in 'secrets.yaml', 'api_keys') {
  $p = Join-Path $DataDir $f
  if (-not (Test-Path $p)) { New-Item -ItemType File $p | Out-Null }
}

if (-not $existing) {
  & $exe service install -config $config
  if ($LASTEXITCODE -ne 0) { throw "service install failed" }
}

# Least privilege: the virtual service account gets Modify on data and volumes only.
icacls $DataDir /grant "${account}:(OI)(CI)M" | Out-Null
foreach ($v in $Volumes) {
  New-Item -ItemType Directory -Force -Path $v | Out-Null
  icacls $v /grant "${account}:(OI)(CI)M" | Out-Null
}
# Secrets readable only by Administrators, SYSTEM and the service.
foreach ($f in 'secrets.yaml', 'api_keys') {
  $p = Join-Path $DataDir $f
  icacls $p /inheritance:r /grant:r "Administrators:F" "SYSTEM:F" "${account}:R" | Out-Null
}

Write-Host "Installed: $(& $exe version)"
Write-Host "Next: initialize each empty volume, e.g.  & '$exe' storage init -config '$config' cctv-d"
Write-Host "Then: Start-Service $svc ; logs in $DataDir\recorder.log and the Application event log."
