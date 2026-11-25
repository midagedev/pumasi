# Served by a pumasi controller. Fetches the controller's build for this
# machine and joins this host to it. Everything it puts here:
#   ~\.local\bin\pumasi.exe, ~\.cache\pumasi\, and a start-at-logon entry
#   (HKCU ...\CurrentVersion\Run, value "pumasi") that runs the agent.
# Run it in a PowerShell window on the machine's desktop: runners started from
# an ssh session or a service have no desktop.
$ErrorActionPreference = 'Stop'
$controller = '{{controller}}'
$pin = '{{pin}}'
if ($args.Count -lt 1) {
  throw 'usage: & ([scriptblock]::Create(...)) <token> [--name NAME] [--cpus N] [--mem 6G] [--desktop]'
}
$token = [string]$args[0]
$rest = @()
if ($args.Count -gt 1) { $rest = @($args[1..($args.Count - 1)]) }

# Windows on ARM runs the x86_64 build.
$file = 'pumasi-windows-amd64.exe'
$dir = Join-Path $HOME '.local\bin'
New-Item -ItemType Directory -Force $dir | Out-Null
$bin = Join-Path $dir 'pumasi.exe'
$new = "$bin.new"

& curl.exe -fsSk --pinnedpubkey $pin -o $new "https://$controller/dl/$file"
if ($LASTEXITCODE -ne 0) { throw "pumasi: could not fetch $file from the controller (curl exit $LASTEXITCODE)" }
$want = & curl.exe -fsSk --pinnedpubkey $pin "https://$controller/dl/$file.sha256"
if ($LASTEXITCODE -ne 0) { throw "pumasi: could not fetch the checksum from the controller (curl exit $LASTEXITCODE)" }
$have = (Get-FileHash -Algorithm SHA256 $new).Hash.ToLower()
if ($have -ne ([string]$want).Trim()) {
  Remove-Item $new -Force
  throw 'pumasi: the download does not match its checksum'
}
# A running pumasi.exe cannot be overwritten, but it can be renamed.
if (Test-Path $bin) {
  Remove-Item "$bin.old" -Force -ErrorAction SilentlyContinue
  Move-Item $bin "$bin.old" -Force
}
Move-Item $new $bin
& $bin join @rest $controller $pin $token
