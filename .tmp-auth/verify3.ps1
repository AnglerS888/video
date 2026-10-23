$ErrorActionPreference = 'Continue'
$base = 'http://127.0.0.1:5393'
$dir = 'E:\goProject\videoDownLoad\.tmp-auth'
$jar = "$dir\c1.txt"
Remove-Item $jar -ErrorAction SilentlyContinue

Write-Host 'E1 save with header, capture cookie'
curl.exe -s -o NUL -w "save=%{http_code}`n" -c $jar -H "X-Access-Password: lockme" -X PUT -H "Content-Type: application/json" --data-binary "@$dir\set_lockme.json" "$base/api/config"
Write-Host 'E2 cookie jar contains vdl_auth?'
(Get-Content $jar -ErrorAction SilentlyContinue | Select-String 'vdl_auth').Line
curl.exe -s -o NUL -w "videos-with-jar=%{http_code}`n" -b $jar "$base/api/videos"

Write-Host 'F lock test: 5 wrong then correct'
1..5 | ForEach-Object {
  curl.exe -s -o NUL -w "wrong$_=%{http_code}`n" -H "X-Access-Password: bad" "$base/api/videos"
}
curl.exe -s -o NUL -w "correct-while-locked=%{http_code}`n" -H "X-Access-Password: lockme" "$base/api/videos"
curl.exe -s -H "X-Access-Password: bad" "$base/api/videos"
Write-Host ''
