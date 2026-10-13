/**
 * 独立"远程控制窗口"：点击详情页「远程控制窗口」按钮弹出的独立浏览器窗口，
 * 只展示远程查看相关布局：视频画面 + 控制栏（麦克风/摄像头/屏幕/画中画/结束）。
 * 复用 remote-view.js 的 WebRTC 控制逻辑（轻量 admin 上下文 window.__admin）。
 */
(function () {
  if (window.__admin) return
  const CONFIG = window.__ADMIN_CONFIG__ || {}
  const q = new URLSearchParams(location.search)
  const deviceId = q.get('deviceId') || ''

  const A = (window.__admin = {
    sock: null,
    token: localStorage.getItem('vr_token') || '',
    remoteView: {}, // remote-view.js 会填充
    esc: (s) => String(s == null ? '' : s),
    toast: (m) => { try { console.log('[rw] ' + m) } catch { /* ignore */ } },
    emit(evt, payload) {
      return new Promise((resolve) => {
        if (!A.sock) return resolve({ ok: false, error: '未连接' })
        // 必须用 socket.io 的 ack 回调（第 3 个参数）：服务端 ack 走的是 socket.io 原生 ack，
        // 不会另外发一个 "ack:<事件名>" 事件。旧写法等的是永远收不到的事件，于是每次 emit
        // 都只能等 12s 超时 → dev:view 失败（设备列表空白）、dev:start 失败（按钮像没反应）。
        let settled = false
        const done = (res) => {
          if (settled) return
          settled = true
          clearTimeout(timer)
          resolve(res || { ok: false, error: '无响应' })
        }
        const timer = setTimeout(() => {
          if (settled) return
          settled = true
          resolve({ ok: false, error: '请求超时' })
        }, 15000)
        A.sock.emit(evt, payload, done)
      })
    }
  })

  const $ = (id) => document.getElementById(id)

  A.sock = io(location.origin, {
    path: CONFIG.path || '/socket.io',
    transports: ['websocket', 'polling'],
    auth: { token: A.token, device: 'web-admin' }
  })

  window.addEventListener('DOMContentLoaded', () => {
    const RV = A.remoteView
    $('rw-close').onclick = () => { try { RV && RV.stop() } catch {} try { window.close() } catch {} }
    $('rv-screen').onclick = () => RV && RV.screen()
    $('rv-stop').onclick = () => RV && RV.stop()
    // 麦克风/摄像头：单击用默认设备，右键/长按弹设备列表（与管理页同一套逻辑）
    if (RV && RV.bindDeviceButtons) RV.bindDeviceButtons()
    document.addEventListener('click', (e) => { if (!e.target.closest('#rv-menu')) RV && RV.hideMenu() })

    A.sock.on('connect', () => {
      $('rw-state').textContent = '在线'
      $('rw-state').classList.add('ok')
      if (deviceId && RV) {
        RV.open({ id: deviceId, name: deviceId, ip: '', os: '' })
        $('rw-dev').textContent = deviceId
      }
    })
    A.sock.on('connect_error', (e) => { $('rw-state').textContent = '连接失败: ' + (e && e.message ? e.message : e) })
  })
})()
