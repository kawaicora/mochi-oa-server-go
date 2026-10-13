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
        const done = (ack) => { A.sock.off('ack:' + evt, done); resolve(ack) }
        A.sock.on('ack:' + evt, done)
        A.sock.emit(evt, payload)
        setTimeout(() => { A.sock.off('ack:' + evt, done); resolve({ ok: false, error: 'timeout' }) }, 12000)
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
    $('rv-cam').onclick = () => RV && RV.cam()
    $('rv-screen').onclick = () => RV && RV.screen()
    $('rv-mic').onclick = () => RV && RV.mic()
    $('rv-stop').onclick = () => RV && RV.stop()
    $('rv-cam').addEventListener('contextmenu', (e) => { e.preventDefault(); RV && RV.cam() })
    $('rv-mic').addEventListener('contextmenu', (e) => { e.preventDefault(); RV && RV.mic() })
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
