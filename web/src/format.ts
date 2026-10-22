/**
 * 体积格式化：统一走 1024 进制，自动选 B/KB/MB/GB/TB。
 * 之前多个页面直接拼 "xxx MB"，几十 GB 的库会显示成 30000.0 MB，很难读。
 *
 * 显示规则：
 *   - 小于 1024 的值保留整数（B）
 *   - 大于等于 100 的值取整（724 MB / 27 GB），否则保留 1 位小数（9.5 GB）
 */
export function fmtBytes(n?: number): string {
  if (!n || n <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  let v = n
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(v >= 100 || i === 0 ? 0 : 1)} ${units[i]}`
}

/** 下载任务/分享页的体积字段都是 MB，这里转成字节后走同一套规则 */
export function fmtMB(mb?: number): string {
  return fmtBytes((mb || 0) * 1024 * 1024)
}
