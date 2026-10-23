$ErrorActionPreference = 'Continue'
$base = 'http://127.0.0.1:5393'
$dir = 'E:\goProject\videoDownLoad\.tmp-auth'
$jar = "$dir\cookies.txt"
Remove-Item $jar -ErrorAction SilentlyContinue

function C([string]$desc, [string[]]$a) {
  $out = & curl.exe -s -w "`nHTTP:%{http_code}" @a
  Write-Host "==== $desc"
  Write-Host $out
}

C '1 status (no pwd)' @("$base/api/auth/status")
C '2 videos (no pwd)' @("$base/api/videos")
C '3 root html (no pwd)' @('-o','NUL','-w','%{http_code} %{content_type}',"$base/")
C '4 set password'  @('-c',$jar,'-b',$jar,'-X','PUT','-H','Content-Type: application/json','--data-binary',"@$dir\setpwd.json","$base/api/config")
C '5 status (no cookie)' @("$base/api/auth/status")
C '6 videos (no pwd -> 401)' @("$base/api/videos")
C '7 videos header pwd' @('-H','X-Access-Password: test123',"$base/api/videos")
C '8 videos query pwd' @("$base/api/videos?password=test123")
C '9 login wrong' @('-X','POST','-H','Content-Type: application/json','--data-binary',"@$dir\login_bad.json","$base/api/auth/login")
C '10 login ok' @('-c',$jar,'-X','POST','-H','Content-Type: application/json','--data-binary',"@$dir\login_ok.json","$base/api/auth/login")
C '11 videos cookie' @('-b',$jar,"$base/api/videos")
C '12 healthz no cookie' @("$base/healthz")
C '13 root no cookie' @('-o','NUL','-w','%{http_code}',"$base/")
C '14 play no cookie' @("$base/play/1")
C '15 config with cookie has auth' @('-b',$jar,"$base/api/config")
Write-Host '==== ui.yml'
Get-Content "$dir\data\ui.yml" | Select-String -Pattern 'auth|password'
