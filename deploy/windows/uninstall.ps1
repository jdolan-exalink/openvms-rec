<# Removes the service registration only. Recordings, database and configuration stay. #>
$ErrorActionPreference = 'Stop'
$svc = Get-Service -Name 'OpenVMSRecorder' -ErrorAction SilentlyContinue
if ($svc -and $svc.Status -ne 'Stopped') { Stop-Service $svc.Name; $svc.WaitForStatus('Stopped', '00:00:45') }
& "$env:ProgramFiles\OpenVMS Recorder\openvms-recorder.exe" service uninstall
Get-Process openvms-tray -ErrorAction SilentlyContinue | Stop-Process -Force
Remove-ItemProperty -Path 'HKLM:\Software\Microsoft\Windows\CurrentVersion\Run' -Name 'OpenVMSRecorderTray' -ErrorAction SilentlyContinue
Write-Host "Recordings and $env:ProgramData\OpenVMS\Recorder were kept."
