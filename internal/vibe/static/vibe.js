// Shared helpers of the vibe harness page, loaded before the components: the API call, the chat's
// markdown and the unified diff parser.
(() => {
  const escapeHTML = (text) => String(text).replace(/[&<>"']/g, (char) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[char])

  // Model output is untrusted: raw HTML in it renders as text, and the sanitize pass below drops
  // event handlers and script URLs marked would otherwise keep.
  window.marked.use({ renderer: { html: ({ text }) => escapeHTML(text) } })

  const safeURL = /^(https?:|mailto:|\/(?!\/)|#|\.{0,2}\/)/i
  const markdownCache = new Map()

  window.Vibe = {
    base: '/_dboss_/vibe',

    // api calls one harness endpoint: a GET without a body, a POST with one (an empty object is
    // fine). The header proves to the server the call came from this page.
    async api(path, body) {
      const options = body === undefined
        ? { headers: { Accept: 'application/json' } }
        : { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Dboss-Vibe': '1' }, body: JSON.stringify(body) }
      const response = await fetch(`${this.base}/api/${path}`, options)
      const data = await response.json().catch(() => null)
      if (response.status === 401) {
        window.location.reload()
      }
      if (!response.ok) throw new Error(data?.error || response.statusText || `HTTP ${response.status}`)
      return data
    },

    escape: escapeHTML,

    markdown(text) {
      if (markdownCache.has(text)) return markdownCache.get(text)
      const template = document.createElement('template')
      template.innerHTML = window.marked.parse(String(text || ''))
      for (const node of template.content.querySelectorAll('*')) {
        for (const attribute of [...node.attributes]) {
          const name = attribute.name.toLowerCase()
          if (name.startsWith('on') || name === 'style' || name === 'srcset' || name === 'formaction') {
            node.removeAttribute(attribute.name)
          } else if ((name === 'href' || name === 'src') && !safeURL.test(attribute.value.trim())) {
            node.removeAttribute(attribute.name)
          }
        }
        if (node.tagName === 'A') {
          node.setAttribute('target', '_blank')
          node.setAttribute('rel', 'noopener noreferrer')
        }
      }
      const html = template.innerHTML
      if (markdownCache.size > 500) markdownCache.clear()
      markdownCache.set(text, html)
      return html
    },

    // parseDiff turns `git diff` output into rows: hunk headers, context, added and removed lines
    // with their old and new line numbers, and notes such as a binary file.
    parseDiff(text) {
      const rows = []
      let oldLine = 0
      let newLine = 0
      let header = true
      for (const line of String(text || '').split('\n')) {
        if (line.startsWith('diff --git ')) {
          header = true
          continue
        }
        if (line.startsWith('@@')) {
          const match = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@(.*)$/.exec(line)
          if (match) {
            oldLine = Number(match[1])
            newLine = Number(match[2])
          }
          header = false
          rows.push({ kind: 'hunk', text: line })
          continue
        }
        if (header) {
          if (line.startsWith('Binary files')) rows.push({ kind: 'note', text: 'Binary file changed' })
          continue
        }
        if (line.startsWith('+')) {
          rows.push({ kind: 'add', new: newLine++, text: line.slice(1) })
        } else if (line.startsWith('-')) {
          rows.push({ kind: 'del', old: oldLine++, text: line.slice(1) })
        } else if (line.startsWith('\\')) {
          rows.push({ kind: 'note', text: line.slice(2) })
        } else if (line !== '') {
          // git writes an empty context line as one space, so only the final newline is bare
          rows.push({ kind: 'ctx', old: oldLine++, new: newLine++, text: line.slice(1) })
        }
      }
      return rows
    },

    // copy puts text on the clipboard. The async clipboard needs a secure page, so a plain http
    // dev host falls back to a selected textarea.
    async copy(text) {
      if (navigator.clipboard && window.isSecureContext) {
        try {
          await navigator.clipboard.writeText(text)
          return true
        } catch {}
      }
      const area = document.createElement('textarea')
      area.value = text
      area.setAttribute('readonly', '')
      area.style.position = 'fixed'
      area.style.opacity = '0'
      document.body.append(area)
      area.select()
      let copied = false
      try {
        copied = document.execCommand('copy')
      } catch {}
      area.remove()
      return copied
    },
  }
})()
