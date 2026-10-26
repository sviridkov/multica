param([Parameter(Mandatory=$true)][string]$BuildDir)
$ErrorActionPreference = 'Stop'
if ($env:GITHUB_ACTIONS -ne 'true' -or $env:RUNNER_ENVIRONMENT -ne 'github-hosted') { throw 'Disposable hosted VM required' }
$labRoot = "C:\multica-cache-lab-$env:GITHUB_RUN_ID-$env:GITHUB_RUN_ATTEMPT"
$labUser = 'cachelabtask'
$labPassword = [guid]::NewGuid().ToString() + 'aA!7'
New-LocalUser -Name $labUser -Password (ConvertTo-SecureString $labPassword -AsPlainText -Force) -AccountNeverExpires | Out-Null
if (!((Get-LocalGroupMember -Group 'Users').Name -contains "$env:COMPUTERNAME\$labUser")) { Add-LocalGroupMember -Group 'Users' -Member $labUser }
if ((Get-LocalGroupMember -Group 'Administrators').Name -contains "$env:COMPUTERNAME\$labUser") { throw 'Task user is administrator' }
New-Item -ItemType Directory -Path $labRoot | Out-Null
& icacls $labRoot /inheritance:r /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' '*S-1-5-32-545:(OI)(CI)RX'
if ($LASTEXITCODE -ne 0) { throw 'ACL bootstrap failed' }
Copy-Item "$BuildDir\producer.exe", "$BuildDir\broker-test.exe" $labRoot
$gitPath = (Get-Command git.exe).Source
@{Root=$labRoot;Producer="$labRoot\producer.exe";Git=$gitPath;TaskUser=$labUser;TaskPassword=$labPassword} | ConvertTo-Json | Set-Content "$labRoot\config.json" -Encoding utf8NoBOM
& icacls "$labRoot\config.json" /inheritance:r /grant:r '*S-1-5-18:F' '*S-1-5-32-544:F'
if ($LASTEXITCODE -ne 0) { throw 'Private fixture credentials ACL failed' }
$runner = @"
`$ErrorActionPreference = 'Stop'
`$nativeExit = 97
`$env:MULTICA_NATIVE_BROKER_CONFIG='$labRoot\config.json'
try {
  & '$labRoot\broker-test.exe' '-test.run=^TestBrokerNative' '-test.v' '-test.timeout=15m' *> '$labRoot\native.log'
  if (`$null -eq `$LASTEXITCODE) { throw 'Test process supplied no exit code' }
  `$nativeExit = `$LASTEXITCODE
} catch {
  `$_ | Out-File '$labRoot\native.log' -Append
} finally {
  `$nativeExit | Set-Content '$labRoot\exit-code.txt'
}
"@
$runner | Set-Content "$labRoot\run.ps1" -Encoding utf8NoBOM
$action = New-ScheduledTaskAction -Execute (Get-Command pwsh.exe).Source -Argument "-NoProfile -File $labRoot\run.ps1" -WorkingDirectory $labRoot
$principal = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest
$taskName = "MulticaCacheLab-$env:GITHUB_RUN_ID"
Register-ScheduledTask -TaskName $taskName -Action $action -Principal $principal | Out-Null
Start-ScheduledTask -TaskName $taskName
$deadline = (Get-Date).AddMinutes(17)
while (!(Test-Path "$labRoot\exit-code.txt")) {
 if ((Get-Date) -gt $deadline) { throw 'Native helper did not finish' }
 Start-Sleep -Seconds 2
}
Copy-Item "$labRoot\native.log" "$BuildDir\native.log"
Get-Content "$BuildDir\native.log"
$statusText = (Get-Content "$labRoot\exit-code.txt" -Raw).Trim()
if ($statusText -notmatch '^\d+$') { throw 'Missing or malformed native exit status' }
$nativeExit = [int]$statusText
Unregister-ScheduledTask -TaskName $taskName -Confirm:$false
exit $nativeExit
