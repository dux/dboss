// The vibe shell: dboss adds this script to every page of a web process with vibe on, and it docks
// the harness panel on the left of the app. The panel is a same-origin frame in a shadow root, so
// the app's CSS and scripts never meet the harness's. The panel talks back with postMessage:
// {dbossVibe: 'open', value: false} collapses it, {dbossVibe: 'wide', value: true} spreads its frame over the site for a diff.
(() => {
  if (window.top !== window || document.getElementById('dboss-vibe')) return

  const KEY = 'dboss-vibe'
  const MIN = 320
  const load = () => {
    try {
      return JSON.parse(localStorage.getItem(KEY)) || {}
    } catch {
      return {}
    }
  }
  const state = { width: 420, open: true, ...load() }
  let wide = false

  const host = document.createElement('div')
  host.id = 'dboss-vibe'
  host.style.cssText = 'position:fixed;top:0;left:0;bottom:0;z-index:2147483647;display:block;margin:0;padding:0;border:0;'
  const root = host.attachShadow({ mode: 'open' })
  root.innerHTML = `
    <style>
      .panel { position: absolute; top: 0; left: 0; bottom: 0; display: flex; background: #fff; box-shadow: 1px 0 0 #e3e5e8, 8px 0 24px rgb(15 23 42 / 8%); }
      .panel[hidden] { display: none; }
      .handle { order: 2; width: 6px; margin-right: -3px; cursor: col-resize; touch-action: none; }
      .wide .handle { display: none; }
      .handle:hover, .dragging .handle { background: rgb(59 130 246 / 35%); }
      iframe { flex: 1; width: 100%; height: 100%; border: 0; background: #fff; }
      .dragging iframe { pointer-events: none; }
      .tab { position: absolute; top: 50%; left: 0; padding: 12px 6px; border: 1px solid #e3e5e8; border-left: 0; border-radius: 0 8px 8px 0; color: #334155; background: #fff; box-shadow: 4px 0 12px rgb(15 23 42 / 10%); font: 600 12px/1 system-ui, sans-serif; letter-spacing: .04em; writing-mode: vertical-rl; transform: translateY(-50%) rotate(180deg); cursor: pointer; }
      .tab[hidden] { display: none; }
    </style>
    <div class="panel"><div class="handle" title="Drag to resize"></div><iframe title="vibe" src="/_dboss_/vibe/panel"></iframe></div>
    <button class="tab" type="button" title="Open the vibe panel">vibe</button>`
  const panel = root.querySelector('.panel')
  const frame = root.querySelector('iframe')
  const tab = root.querySelector('.tab')

  const width = () => Math.max(MIN, Math.min(state.width, window.innerWidth - 160))

  // Fixed elements ignore the page margin, so the ones that reach under the panel (a top bar, a
  // toast corner) are moved out by hand, and put back when it closes.
  const moved = new Map()
  const unpin = () => {
    for (const [element, style] of moved) element.style.cssText = style
    moved.clear()
  }
  const pin = (gap) => {
    unpin()
    if (!gap || !document.body) return
    const edge = window.innerWidth
    for (const element of document.body.querySelectorAll('*')) {
      if (element === host) continue
      const style = getComputedStyle(element)
      if (style.position !== 'fixed') continue
      const box = element.getBoundingClientRect()
      if (!box.width || box.left >= gap) continue
      // Shift it right by the panel and cap its width, so a full-width bar ends at the viewport edge.
      const left = parseFloat(style.left) || 0
      moved.set(element, element.style.cssText)
      element.style.setProperty('left', `${gap + left}px`, 'important')
      element.style.setProperty('max-width', `${edge - gap - left}px`, 'important')
    }
  }

  const render = () => {
    panel.hidden = !state.open
    tab.hidden = state.open
    // A diff covers the site; the page keeps its margin underneath, so closing it does not reflow.
    panel.style.width = wide ? '100vw' : `${width()}px`
    panel.classList.toggle('wide', wide)
    // Normal-flow content moves aside; fixed elements are moved by pin.
    document.documentElement.style.setProperty('margin-left', state.open ? `${width()}px` : '0px', 'important')
    pin(state.open ? width() : 0)
  }
  let pending = 0
  const renderSoon = () => {
    window.cancelAnimationFrame(pending)
    pending = window.requestAnimationFrame(render)
  }

  const save = () => {
    try {
      localStorage.setItem(KEY, JSON.stringify(state))
    } catch {}
  }

  tab.addEventListener('click', () => {
    state.open = true
    save()
    render()
  })

  root.querySelector('.handle').addEventListener('pointerdown', (event) => {
    event.preventDefault()
    panel.classList.add('dragging')
    const move = (moved) => {
      state.width = Math.round(Math.max(MIN, moved.clientX))
      renderSoon()
    }
    const up = () => {
      panel.classList.remove('dragging')
      window.removeEventListener('pointermove', move)
      window.removeEventListener('pointerup', up)
      save()
    }
    window.addEventListener('pointermove', move)
    window.addEventListener('pointerup', up)
  })

  window.addEventListener('message', (event) => {
    if (event.origin !== window.location.origin || event.source !== frame.contentWindow) return
    const { dbossVibe: kind, value } = event.data || {}
    if (kind === 'open') {
      state.open = Boolean(value)
      save()
    } else if (kind === 'wide') {
      wide = Boolean(value)
    } else {
      return
    }
    render()
  })
  window.addEventListener('resize', renderSoon)

  const mount = () => {
    if (!host.isConnected) document.body.appendChild(host)
  }
  // An app that swaps the whole body (pjax, morphing) must not take the panel with it, and new
  // fixed elements need moving too.
  new MutationObserver((records) => {
    mount()
    if (records.some((record) => !host.contains(record.target))) renderSoon()
  }).observe(document.documentElement, { childList: true, subtree: true })
  if (document.body) mount()
  else document.addEventListener('DOMContentLoaded', mount)
  render()
})()
