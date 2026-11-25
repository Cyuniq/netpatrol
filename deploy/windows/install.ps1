[CmdletBinding()]
param(
    [ValidateRange(1, 65535)]
    [int] $Port = 8080,
    [switch] $Lan
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$serviceName = 'NetPatrol'
$principal = [Security.Principal.WindowsPrincipal]::new([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    $arguments = @('-NoLogo', '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', ('"{0}"' -f $PSCommandPath))
    if ($PSBoundParameters.ContainsKey('Port')) { $arguments += @('-Port', $Port) }
    if ($Lan) { $arguments += '-Lan' }
    $engine = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
    $process = Start-Process -FilePath $engine -ArgumentList $arguments -Verb RunAs -WindowStyle Hidden -Wait -PassThru
    if ($process.ExitCode -eq 0) {
        Write-Host 'NetPatrol is running. Use http://127.0.0.1:8080/devices or your configured port.'
    } else {
        Write-Host 'See the Windows event log or %ProgramData%\NetPatrol\data\netpatrol.log for startup errors.'
    }
    exit $process.ExitCode
}
if (-not [Environment]::Is64BitOperatingSystem -or $env:PROCESSOR_ARCHITECTURE -ne 'AMD64') {
    throw 'This installer requires native x64 Windows PowerShell.'
}

function Invoke-Native {
    param([string] $Program, [string[]] $Arguments)
    & $Program @Arguments | Out-Host
    if ($LASTEXITCODE -ne 0) {
        throw "$Program failed with exit code $LASTEXITCODE."
    }
}

$root = Join-Path $env:ProgramData 'NetPatrol'
$data = Join-Path $root 'data'
$programDir = Join-Path $env:ProgramFiles 'NetPatrol'
$program = Join-Path $programDir 'netpatrol.exe'
$source = Join-Path $PSScriptRoot 'netpatrol.exe'
$database = Join-Path $data 'netpatrol.db'
$config = Join-Path $root 'netpatrol.env'
$logFile = Join-Path $data 'netpatrol.log'
$registry = 'HKLM:\SYSTEM\CurrentControlSet\Services\NetPatrol'
$mutex = [Threading.Mutex]::new($false, 'Global\NetPatrolInstaller')
$locked = $false

try {
    try { $locked = $mutex.WaitOne(0) }
    catch [Threading.AbandonedMutexException] { $locked = $true }
    if (-not $locked) { throw 'Another NetPatrol installation is running.' }

    $service = Get-Service -Name $serviceName -ErrorAction SilentlyContinue
    $existing = $null -ne $service
    $wasRunning = $existing -and $service.Status -eq 'Running'
    $binding = if ($Lan) { "0.0.0.0:$Port" } else { "127.0.0.1:$Port" }
    if ($existing) {
        $image = (Get-ItemProperty -LiteralPath $registry -Name ImagePath).ImagePath
        if (!$image.StartsWith('"' + $program + '" ', [StringComparison]::OrdinalIgnoreCase) -or
            !$image.Contains(' -db "' + $database + '"') -or
            -not ($image -match ' -listen "(?<binding>(?:127\.0\.0\.1|0\.0\.0\.0):(?<port>\d+))"')) {
            throw 'An existing NetPatrol service uses a different installation layout.'
        }
        $binding = $Matches.binding
        $existingPort = [int] $Matches.port
        if (($PSBoundParameters.ContainsKey('Port') -and $Port -ne $existingPort) -or
            ($Lan -and $binding.StartsWith('127.'))) {
            throw 'Upgrades keep the current listener. Use the existing options when rerunning the installer.'
        }
        $Port = $existingPort
    }

    foreach ($directory in @($root, $data, $programDir)) {
        if (Test-Path -LiteralPath $directory) {
            $item = Get-Item -LiteralPath $directory -Force
            if (-not $item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
                throw "Unexpected installation path: $directory"
            }
        }
    }
    # LocalService can read the configuration and write the database and logs.
    if (-not (Test-Path -LiteralPath $root)) {
        New-Item -ItemType Directory -Path $root | Out-Null
        Invoke-Native icacls.exe @($root, '/inheritance:r', '/grant:r',
            '*S-1-5-18:(OI)(CI)F', '*S-1-5-32-544:(OI)(CI)F', '*S-1-5-19:(OI)(CI)RX')
    }
    if (-not (Test-Path -LiteralPath $data)) {
        New-Item -ItemType Directory -Path $data | Out-Null
        Invoke-Native icacls.exe @($data, '/grant:r', '*S-1-5-19:(OI)(CI)M')
    }
    if (-not (Test-Path -LiteralPath $programDir)) {
        New-Item -ItemType Directory -Path $programDir | Out-Null
    }
    if (-not (Test-Path -LiteralPath $config)) {
        Set-Content -LiteralPath $config -Encoding Ascii -Value @(
            'NETPATROL_NO_AUTH=1', 'NETPATROL_ADMIN_PASSWORD=', 'NETPATROL_NO_AUTH_CIDRS=')
    }

    $backup = $null
    $oldProgram = $null
    if (Test-Path -LiteralPath $program -PathType Leaf) {
        $backup = Join-Path $root ('backup-' + [guid]::NewGuid().ToString('N'))
        New-Item -ItemType Directory -Path $backup | Out-Null
        $oldProgram = Join-Path $backup 'netpatrol.exe'
        Copy-Item -LiteralPath $program -Destination $oldProgram
        if (Test-Path -LiteralPath $database -PathType Leaf) {
            Invoke-Native $program @('-db', $database, '-backup-to', (Join-Path $backup 'netpatrol.db'))
        }
    } elseif ($existing) {
        throw 'The program for the existing NetPatrol service is missing.'
    }

    $created = $false
    $replaced = $false
    $next = Join-Path $programDir ('netpatrol-' + [guid]::NewGuid().ToString('N') + '.exe')
    try {
        Copy-Item -LiteralPath $source -Destination $next
        if ($wasRunning) {
            Stop-Service -Name $serviceName
            $service.WaitForStatus('Stopped', [TimeSpan]::FromSeconds(25))
        }
        if ($oldProgram) {
            [IO.File]::Replace($next, $program, [NullString]::Value)
        } else {
            [IO.File]::Move($next, $program)
        }
        $replaced = $true

        if (-not $existing) {
            $attributes = @{
                Name = $serviceName
                BinaryPathName = '"{0}" -listen "{1}" -db "{2}" -env-file "{3}" -log-file "{4}"' -f $program, $binding, $database, $config, $logFile
                DisplayName = 'NetPatrol'
                Description = 'NetPatrol device monitoring'
                StartupType = 'Manual'
            }
            New-Service @attributes | Out-Null
            $created = $true
            Invoke-Native sc.exe @('config', $serviceName, 'obj=', 'NT AUTHORITY\LocalService')
        }
        Start-Service -Name $serviceName
        $service = Get-Service -Name $serviceName
        $service.WaitForStatus('Running', [TimeSpan]::FromSeconds(25))

        $url = "http://127.0.0.1:$Port"
        try {
            $check = Invoke-WebRequest -Uri "$url/diagnostics.json" -UseBasicParsing -TimeoutSec 8
            if ($check.StatusCode -ne 200 -or $check.Content -notmatch '"scheduled_devices"') {
                throw 'NetPatrol HTTP verification failed.'
            }
        } catch {
            $response = $_.Exception.PSObject.Properties['Response']
            if (!$response -or !$response.Value -or [int] $response.Value.StatusCode -ne 401) { throw }
        }
        if ($created) {
            Set-Service -Name $serviceName -StartupType Automatic
            if ($Lan) {
                $firewall = @{
                    Name = 'NetPatrol-Web'
                    DisplayName = 'NetPatrol Web (local subnet)'
                    Direction = 'Inbound'
                    Action = 'Allow'
                    Protocol = 'TCP'
                    LocalPort = $Port
                    Program = $program
                    RemoteAddress = 'LocalSubnet'
                }
                New-NetFirewallRule @firewall | Out-Null
            }
        }
    } catch {
        $startupError = $_
        if ($created -or $replaced -or $wasRunning) {
            Stop-Service -Name $serviceName -ErrorAction SilentlyContinue
        }
        if ($created) { & sc.exe delete $serviceName | Out-Host }
        if ($oldProgram -and $replaced) {
            Copy-Item -LiteralPath $oldProgram -Destination $program -Force
        }
        if ($wasRunning) { Start-Service -Name $serviceName }
        throw $startupError
    } finally {
        if (Test-Path -LiteralPath $next) { Remove-Item -LiteralPath $next }
    }
    Write-Host "NetPatrol is running at $url/devices"
    Write-Host "Data: $database"
    if ($backup) { Write-Host "Upgrade backup: $backup" }
} finally {
    if ($locked) { $mutex.ReleaseMutex() }
    $mutex.Dispose()
}
