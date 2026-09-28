// Shared value formatters for the console, mirroring internal/humanize on the Go side. Loaded
// before the components in index.html, so templates and classes call Human.* instead of carrying
// their own copy of a size, a count or an age.
window.Human = {
  // hasTime is false for an empty timestamp or Go's zero time, which rides JSON as 0001-01-01.
  hasTime(value) {
    return Boolean(value) && !String(value).startsWith('0001-')
  },

  number(value) {
    return new Intl.NumberFormat().format(Math.round(Number(value || 0)))
  },

  // bytes renders a size as 512 B, 1.5 KB, 2.0 GB; a zero or missing size is "-".
  bytes(value) {
    const amount = Math.round(Number(value || 0))
    if (!amount) return '-'
    const units = ['B', 'KB', 'MB', 'GB', 'TB']
    let index = 0
    let size = amount
    while (size >= 1024 && index < units.length - 1) {
      size /= 1024
      index++
    }
    return `${size.toFixed(index === 0 ? 0 : 1)} ${units[index]}`
  },

  duration(seconds) {
    const total = Number(seconds || 0)
    if (!total) return '-'
    const days = Math.floor(total / 86400)
    const hours = Math.floor((total % 86400) / 3600)
    const minutes = Math.floor((total % 3600) / 60)
    if (days) return `${days}d ${hours}h`
    if (hours) return `${hours}h ${minutes}m`
    return `${minutes}m`
  },

  // ago renders an ISO timestamp as "just now", "42s ago", "3min ago", "2h ago", "5d ago", "3mo
  // ago" or "2y ago". A missing or invalid value is "-".
  ago(value) {
    if (!value) return '-'
    const then = new Date(value).getTime()
    if (isNaN(then)) return '-'
    const seconds = Math.max(0, Math.floor((Date.now() - then) / 1000))
    if (seconds < 10) return 'just now'
    if (seconds < 60) return `${seconds}s ago`
    const minutes = Math.floor(seconds / 60)
    if (minutes < 60) return `${minutes}min ago`
    const hours = Math.floor(minutes / 60)
    if (hours < 24) return `${hours}h ago`
    const days = Math.floor(hours / 24)
    if (days < 30) return `${days}d ago`
    const months = Math.floor(days / 30)
    if (months < 12) return `${months}mo ago`
    return `${Math.floor(months / 12)}y ago`
  },

  stamp(value) {
    if (!value) return '-'
    const date = new Date(value)
    if (isNaN(date.getTime())) return '-'
    return date.toLocaleString()
  },

  // time is the short stamp a dense list uses: "Sep 28, 10:30 AM".
  time(value) {
    if (!value) return ''
    const date = new Date(value)
    if (isNaN(date.getTime())) return ''
    return date.toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })
  },

  percent(value) {
    return `${Math.round(Number(value || 0) * 1000) / 10}%`
  },
}
