$ErrorActionPreference = 'Continue'
$base = 'http://127.0.0.1:5393'
$dir = 'E:\goProject\videoDownLoad\.tmp-auth'
$jarA = "$dir\a.txt"
$jarB = "$dir\b.txt"
Remove-Item $jarA,$jarB -ErrorAction SilentlyContinue

function S([string]$desc, [string[]]$a) {
  $out = & curl.exe -s -o NUL -w "%{http_code}" @a
  Write-Host "$desc -> $out"
}

# A. 保存配置时给当前浏览器续 Cookie（用请求头密码授权，只收集保存响应下发的 Cookie）
S 'A1 PUT set test123 (header pwd, jarA)' @('-c',$jarA,'-H','X-Access-Password: test123','-X','PUT','-H','Content-Type: application/json','--data-binary',"@$dir\setpwd.json","$base/api/config")
S 'A2 videos with jarA only' @('-b',$jarA,"$base/api/videos")

# B. 改密码后旧 Cookie 失效、新 Cookie 可用
S 'B1 PUT change test456 (header pwd, jarB)' @('-c',$jarB,'-H','X-Access-Password: test123','-X','PUT','-H','Content-Type: application/json','--data-binary',"@$dir\set_test456.json","$base/api/config")
S 'B2 videos old jarA (expect 401)' @('-b',$jarA,"$base/api/videos")
S 'B3 videos new jarB (expect 200)' @('-b',$jarB,"$base/api/videos")
S 'B4 login old pwd (expect 401)' @('-X','POST','-H','Content-Type: application/json','--data-binary',"@$dir\login_ok.json","$base/api/auth/login")
& curl.exe -s -X POST -H 'Content-Type: application/json' -d '{"password":"test456"}' "$base/api/auth/login" | Out-Null

# C. 关闭密码
S 'C1 PUT disable (jarB)' @('-b',$jarB,'-X','PUT','-H','Content-Type: application/json','--data-binary',"@$dir\unset.json","$base/api/config")
S 'C2 videos no cookie (expect 200)' @("$base/api/videos")
& curl.exe -s "$base/api/auth/status"
Write-Host ''

# D. 重新启用并测试限流（同 IP 连续错 5 次锁 5 分钟）
S 'D1 PUT set lockme (no auth needed)' @('-X','PUT','-H','Content-Type: application/json','--data-binary',"@$dir\set_lockme.json","$base/api/config")
for ($i=1; $i -le 5; $i++) {
  S "D2.$i wrong pwd" @('-H','X-Access-Password: bad',"$base/api/videos")
}
S 'D3 correct pwd while locked (expect 429)' @('-H','X-Access-Password: lockme',"$base/api/videos")
