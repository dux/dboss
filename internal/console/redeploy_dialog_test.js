import { expect, test } from 'bun:test'

const source = await Bun.file(new URL('./static/fez/db-redeploy-dialog.fez', import.meta.url)).text()
const script = source.match(/<script>([\s\S]*?)<\/script>/)[1]
const preview = { branch: 'vibe', upstream: 'origin/vibe', local: { hash: 'abc', subject: 'Local' }, remote: { hash: 'def', subject: 'Remote' }, ahead: 1, behind: 2 }

function fixture(api = async () => preview) {
  const calls = []
  const events = {}
  const Dialog = new Function('Dboss', `return (${script})`)({
    api: (path, options) => {
      calls.push({ path, options })
      return api(path, options)
    },
  })
  const dialog = new Dialog()
  dialog.state = {}
  dialog.root = { open: false, setAttribute() {}, showModal() { this.open = true }, close() { this.open = false } }
  dialog.on = (target, name, callback) => { events[name] = callback }
  dialog.init()
  dialog.onMount()
  return { dialog, calls, events }
}

test('The dialog loads local and remote commits before confirmation is possible', async () => {
  let reply
  const { dialog, calls } = fixture(() => new Promise((resolve) => { reply = resolve }))
  const answer = dialog.confirm('my app')
  expect(dialog.root.open).toBe(true)
  expect(dialog.state.loading).toBe(true)
  expect(calls[0].path).toBe('/ui/apps/deploy-preview?app=my%20app')
  dialog.finish(true)
  expect(dialog.root.open).toBe(true)
  reply(preview)
  await Promise.resolve()
  expect(dialog.state.preview).toEqual(preview)
  expect(dialog.state.loading).toBe(false)
  dialog.submit({ preventDefault() {} })
  expect(await answer).toBe(true)
  expect(dialog.root.open).toBe(false)
  expect(calls).toHaveLength(1)
})

test('Cancellation aborts the preview and ignores late results', async () => {
  let reply
  const { dialog, calls } = fixture(() => new Promise((resolve) => { reply = resolve }))
  const answer = dialog.confirm('shop')
  dialog.finish(false)
  expect(await answer).toBe(false)
  expect(calls[0].options.signal.aborted).toBe(true)
  reply(preview)
  await Promise.resolve()
  expect(dialog.state.preview).toBe(null)
})

test('A preview failure disables confirmation and can be retried', async () => {
  let failed = true
  const { dialog } = fixture(async () => {
    if (failed) throw new Error('Remote is unreachable')
    return preview
  })
  const answer = dialog.confirm('shop')
  await Promise.resolve()
  expect(dialog.state.error).toBe('Remote is unreachable')
  dialog.finish(true)
  expect(dialog.root.open).toBe(true)
  failed = false
  await dialog.load()
  expect(dialog.state.error).toBe('')
  dialog.finish(true)
  expect(await answer).toBe(true)
})

test('Only one confirmation dialog can be open and destruction cancels it', async () => {
  const { dialog } = fixture()
  const answer = dialog.confirm('shop')
  expect(await dialog.confirm('other')).toBe(false)
  expect(dialog.state.app).toBe('shop')
  dialog.onDestroy()
  expect(await answer).toBe(false)
})

test('A cancelled preview cannot overwrite the next app comparison', async () => {
  let oldReply
  const { dialog } = fixture((path) => path.endsWith('shop') ? new Promise((resolve) => { oldReply = resolve }) : Promise.resolve(preview))
  const oldAnswer = dialog.confirm('shop')
  dialog.finish(false)
  await oldAnswer
  const nextAnswer = dialog.confirm('other')
  await Promise.resolve()
  oldReply({ ...preview, branch: 'old' })
  await Promise.resolve()
  expect(dialog.state.preview.branch).toBe('vibe')
  dialog.finish(false)
  await nextAnswer
})

test('Escape cancels and stops page shortcuts from swallowing the dialog close', async () => {
  const { dialog, events } = fixture()
  const answer = dialog.confirm('shop')
  let prevented = false
  let stopped = false
  events.keydown({ key: 'Escape', preventDefault() { prevented = true }, stopPropagation() { stopped = true } })
  expect(await answer).toBe(false)
  expect(dialog.root.open).toBe(false)
  expect(prevented).toBe(true)
  expect(stopped).toBe(true)
})
