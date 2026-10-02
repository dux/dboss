import { expect, test } from 'bun:test'

const source = await Bun.file(new URL('./static/fez/db-app-card.fez', import.meta.url)).text()
const script = source.match(/<script>([\s\S]*?)<\/script>/)[1]

function fixture(app = {}, api, confirm = async () => true) {
  const messages = []
  const timers = []
  const calls = []
  const deploy = { name: 'deploy', restart: true, running: false, last_exit: 0 }
  const dboss = {
    api: async (path, options) => {
      calls.push({ path, options })
      return api ? api(path, options) : { hooks: [deploy] }
    },
    reload: async () => {},
  }
  const Card = new Function('Dboss', 'Toast', 'Human', 'RedeployDialog', `return (${script})`)(
    dboss,
    { show: (message, error = false) => messages.push({ message, error }) },
    { hasTime: (value) => Boolean(value) },
    { confirm },
  )
  const card = new Card()
  card.state = {}
  card.globalState = {}
  card.props = { app: { name: 'shop', state: 'running', git_connected: true, hooks: [deploy], ...app } }
  card.isConnected = true
  card.setTimeout = (callback) => { timers.push(callback) }
  card.init()
  card.beforeRender()
  return { card, messages, timers, calls }
}

test('Redeploy requires a connected repo and an enabled, idle deploy hook', async () => {
  for (const app of [
    { git_connected: false },
    { hooks: [] },
    { hooks: [{ name: 'deploy', disabled: true }] },
    { hooks: [{ name: 'deploy', running: true }] },
    { hooks: [{ name: 'deploy', restarting: true }] },
    { state: 'starting' },
    { state: 'stopping' },
    { rolling: true },
  ]) {
    const { card, calls } = fixture(app)
    await card.redeploy()
    expect(calls).toHaveLength(0)
  }
  const { card } = fixture({ hooks: [] })
  expect(card.redeployTitle()).toContain('Git checkout')
})

test('Redeploy posts once and stays busy through the pull and restart', async () => {
  let accept
  let status = { name: 'deploy', running: true, last_start: '2026-10-02T12:00:00Z', output: 'Pulling...' }
  const { card, calls, timers, messages } = fixture({}, (path) => {
    if (path === '/ui/hooks/run') return new Promise((resolve) => { accept = resolve })
    return { hooks: [status] }
  })
  const pending = card.redeploy()
  await Promise.resolve()
  expect(card.state.pending).toBe(true)
  await card.redeploy()
  expect(calls).toHaveLength(1)
  expect(JSON.parse(calls[0].options.body)).toEqual({ app: 'shop', hook: 'deploy' })
  accept({ ok: true })
  await pending
  card.beforeRender()
  expect(card.state.deployBusy).toBe(true)
  expect(card.state.redeployDisabled).toBe(true)
  expect(card.state.deploy.output).toBe('Pulling...')
  status = { ...status, running: false, restarting: true }
  await timers.pop()()
  card.beforeRender()
  expect(card.lastRun(card.state.deploy)).toBe('restarting')
  expect(card.state.deployBusy).toBe(true)
  await card.redeploy()
  expect(calls.filter((call) => call.path === '/ui/hooks/run')).toHaveLength(1)
  status = { ...status, restarting: false, last_exit: 0, output: 'Already up to date.' }
  await timers.pop()()
  card.beforeRender()
  expect(card.state.deployBusy).toBe(false)
  expect(card.state.redeployDisabled).toBe(false)
  expect(card.state.deploy.output).toBe('Already up to date.')
  expect(messages.at(-1)).toEqual({ message: 'shop: redeployed', error: false })
})

test('Redeploy waits for confirmation and cancellation runs no hook', async () => {
  let answer
  let app
  const { card, calls } = fixture({}, undefined, (name) => {
    app = name
    return new Promise((resolve) => { answer = resolve })
  })
  const pending = card.redeploy()
  expect(app).toBe('shop')
  expect(card.state.deployConfirming).toBe(true)
  expect(calls).toHaveLength(0)
  await card.redeploy()
  expect(calls).toHaveLength(0)
  answer(false)
  await pending
  expect(calls).toHaveLength(0)
  expect(card.state.deployConfirming).toBe(false)
})

test('Confirmation starts one hook immediately without another prompt', async () => {
  let confirmations = 0
  const { card, calls } = fixture({}, undefined, async () => {
    confirmations++
    return true
  })
  await card.redeploy()
  expect(confirmations).toBe(1)
  expect(calls.filter((call) => call.path === '/ui/hooks/run')).toHaveLength(1)
})

test('Confirmation cannot deploy a card that was removed or became busy', async () => {
  for (const change of [
    (card) => { card.isConnected = false },
    (card) => { card.state.redeployDisabled = true },
  ]) {
    const { card, calls } = fixture({}, undefined, async () => {
      change(card)
      return true
    })
    await card.redeploy()
    expect(calls).toHaveLength(0)
  }
})

test('A rejected deploy gives the button back and reports the error', async () => {
  const { card, messages } = fixture({}, (path) => {
    if (path === '/ui/hooks/run') throw new Error('deploy hook is disabled')
    return { hooks: [{ name: 'deploy', disabled: true }] }
  })
  await card.redeploy()
  expect(card.state.pending).toBe(false)
  expect(card.state.deployWatching).toBe(false)
  expect(messages.at(-1)).toEqual({ message: 'deploy hook is disabled', error: true })
})

test('Failed deployments retain output and report the actual exit result', async () => {
  for (const last_error of ['Not possible to fast-forward', '']) {
    const status = { name: 'deploy', last_exit: 1, last_error, output: 'fatal: divergent branches', last_start: '2026-10-02T12:00:00Z' }
    const { card, messages } = fixture({}, () => ({ hooks: [status] }))
    await card.redeploy()
    card.beforeRender()
    expect(card.state.deployBusy).toBe(false)
    expect(card.state.deploy.output).toBe(status.output)
    expect(messages.at(-1)).toEqual({ message: `shop: redeploy failed: ${last_error || 'exit 1'}`, error: true })
  }
})

test('A status response from before Redeploy cannot finish the new deployment', async () => {
  let stale
  let reads = 0
  const { card } = fixture({}, (path) => {
    if (path === '/ui/hooks/run') return { ok: true }
    if (++reads === 1) return new Promise((resolve) => { stale = resolve })
    return { hooks: [{ name: 'deploy', running: true }] }
  })
  const old = card.loadHooks()
  await card.redeploy()
  stale({ hooks: [{ name: 'deploy', running: false, last_exit: 0 }] })
  await old
  expect(card.state.deployWatching).toBe(true)
  expect(card.state.hookMeta.deploy.running).toBe(true)
})

test('A status outage keeps the deployment busy and retries', async () => {
  let unavailable = true
  const { card, timers } = fixture({}, (path) => {
    if (path === '/ui/hooks/run') return { ok: true }
    if (unavailable) throw new Error('Daemon unavailable')
    return { hooks: [{ name: 'deploy', running: false, last_exit: 0 }] }
  })
  await card.redeploy()
  card.beforeRender()
  expect(card.state.deployBusy).toBe(true)
  expect(card.state.hookError).toBe('Daemon unavailable')
  unavailable = false
  await timers.pop()()
  expect(card.state.deployWatching).toBe(false)
  expect(card.state.hookError).toBe('')
})

test('A deploy started elsewhere is monitored when the card loads', async () => {
  const { card, timers } = fixture({}, () => ({ hooks: [{ name: 'deploy', restarting: true }] }))
  await card.loadHooks()
  card.beforeRender()
  expect(card.state.deployBusy).toBe(true)
  expect(timers).toHaveLength(1)
})

test('An unmounted card ignores late status responses', async () => {
  let resolve
  const { card, timers } = fixture({}, () => new Promise((done) => { resolve = done }))
  const request = card.loadHooks()
  card.isConnected = false
  resolve({ hooks: [{ name: 'deploy', running: true }] })
  await request
  expect(card.state.hookMeta).toEqual({})
  expect(timers).toHaveLength(0)
})
