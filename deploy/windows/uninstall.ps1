<# Removes the service registration only. Recordings, database and configuration stay. #>
$ErrorActionPreference = 'Stop'
$svc = Get-Service -Name 'OpenVMSRecorder' -ErrorAction SilentlyContinue
if ($svc -and $svc.Status -ne 'Stopped') { Stop-Service $svc.Name; $svc.WaitForStatus('Stopped', '00:00:45') }
& "$env:ProgramFiles\OpenVMS Recorder\openvms-recorder.exe" service uninstall
Write-Host "Recordings and $env:ProgramData\OpenVMS\Recorder were kept."
