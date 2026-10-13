/**
 * 客户端接口一致性探测（只读；不改动客户端代码，仅借用它的 socket.io-client 依赖）。
 *
 * 复刻 mochi-oa 客户端 main 进程 net/server-client.ts 的登录流程：
 *   1. socket.io 连接（auth: {device}，transports: ['websocket','polling']）
 *   2. auth:getPublicKey  → 期望 ack { ok, data: { publicKey } }
 *   3. 用 publicKey 做 RSA PKCS1 加密 JSON → base64
 *   4. auth:login({ enc, device, clientInfo }) → 期望 ack { ok, data: { token, user, companies, session } }
 *
 * 用法：node probe-login.mjs [baseUrl] [account] [password]
 */
import { createRequire } from 'node:module'

const CLIENT_ROOT = 'D:/___SYSTEM_PATH/source/repos/mochi-oa'
const require = createRequire(CLIENT_ROOT + '/package.json')
const { io } = require('socket.io-client')
const crypto = require('node:crypto')

const baseUrl = process.argv[2] || 'http://127.0.0.1:3001'
const account = process.argv[3] || 'dsh_probe_user'
const password = process.argv[4] || 'Probe#12345'

const log = (...a) => console.log('[probe]', ...a)

/** 与客户端 server-client.ts 的 normalizeAck 完全一致：把顶层字段收进 data */
function normalizeAck(raw) {
  if (!raw || typeof raw !== 'object') return { ok: false, error: '无响应' }
  const data = {}
  for (const k of Object.keys(raw)) {
    if (k === 'ok' || k === 'error') continue
    data[k] = raw[k]
  }
  return {
    ok: raw.ok === true,
    error: typeof raw.error === 'string' && raw.error ? raw.error : undefined,
    data: Object.keys(data).length ? data : undefined
  }
}

function emitWithAck(socket, event, payload, timeoutMs = 10000) {
  return new Promise((resolve) => {
    const t0 = Date.now()
    let settled = false
    const timer = setTimeout(() => {
      if (settled) return
      settled = true
      resolve({ ok: false, error: `请求超时(${timeoutMs}ms)` })
    }, timeoutMs)
    socket.emit(event, payload, (raw) => {
      if (settled) return
      settled = true
      clearTimeout(timer)
      const ack = normalizeAck(raw)
      log(`ack ${event} raw=${JSON.stringify(raw).slice(0, 160)}  ${Date.now() - t0}ms`)
      resolve(ack)
    })
  })
}

function encryptCreds(publicKeyPem, payload) {
  const buf = Buffer.from(JSON.stringify(payload), 'utf8')
  const enc = crypto.publicEncrypt({ key: publicKeyPem, padding: crypto.constants.RSA_PKCS1_PADDING }, buf)
  return enc.toString('base64')
}

const socket = io(baseUrl, {
  auth: { device: 'dsh-probe' },
  transports: ['websocket', 'polling'],
  reconnection: false,
  timeout: 8000
})

const result = { baseUrl, steps: [] }
const step = (name, ok, detail) => {
  result.steps.push({ name, ok, detail })
  log(`${ok ? 'PASS' : 'FAIL'} ${name}${detail !== undefined ? ' :: ' + JSON.stringify(detail).slice(0, 300) : ''}`)
}

socket.on('connect', async () => {
  step('socket.io 连接', true, { sid: socket.id, transport: socket.io.engine.transport.name, eio: socket.io.engine.protocol })
  try {
    // 1) 公钥
    const pk = await emitWithAck(socket, 'auth:getPublicKey', {})
    const pem = pk?.data?.publicKey
    step('auth:getPublicKey 返回公钥', !!pem, { ok: pk?.ok, len: pem?.length ?? 0, head: pem ? pem.slice(0, 32) : null })
    if (!pem) return finish()

    // 2) 先尝试注册（幂等：已存在则忽略），保证有可登录账号
    //    注意：注册用 username 字段、登录用 account 字段（与客户端 register(): {username,password} / login(): {account,password} 一致）
    const reg = await emitWithAck(socket, 'auth:register', {
      enc: encryptCreds(pem, { username: account, password, nick: 'DSH Probe' }),
      device: 'dsh-probe'
    })
    step('auth:register（便于后续登录）', !!reg?.ok, { ok: reg?.ok, error: reg?.error })

    // 3) 登录
    const ci = { ip: '127.0.0.1', location: 'probe', os: process.platform }
    const login = await emitWithAck(socket, 'auth:login', {
      enc: encryptCreds(pem, { account, password }),
      device: 'dsh-probe',
      clientInfo: ci
    })
    step('auth:login', !!login?.ok, {
      ok: login?.ok,
      error: login?.error,
      dataKeys: login?.data ? Object.keys(login.data) : null,
      userKeys: login?.data?.user ? Object.keys(login.data.user) : null,
      hasToken: !!login?.data?.token
    })
    result.loginAck = login
  } catch (e) {
    step('异常', false, String(e && e.stack ? e.stack : e))
  }
  finish()
})

socket.on('connect_error', (e) => step('socket.io 连接', false, String(e && e.message ? e.message : e)))
socket.on('disconnect', (r) => log('disconnect:', r))
socket.onAny((ev, ...args) => {
  if (typeof args[args.length - 1] !== 'function') log(`rx ${ev}`, JSON.stringify(args[0]).slice(0, 200))
})

function finish() {
  console.log('__PROBE_RESULT__' + JSON.stringify(result))
  socket.close()
  setTimeout(() => process.exit(0), 200)
}

setTimeout(() => {
  step('总超时', false, '15s 内未完成')
  finish()
}, 15000)
