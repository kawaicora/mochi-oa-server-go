/**
 * 远程设备控制 —— 控制端（web 管理"已登录电脑控制"面板）
 *
 * 点进一台在线远程客户端设备后：
 *   dev:view       → 绑定控制器，取 iceServers
 *   dev:enumerate  → 被控端枚举摄像头/麦克风，经 dev:devices 上报
 *   dev:start      → 开始采集（camera / screen / mic），WebRTC 接收流显示
 *   dev:stop       → 停止
 *   信令 dev:signal（被控端 sender offer → 控制端 answer + 双向 ICE trickle）
 */
;(function () {
  const A = window.__admin
  if (!A) return
  const RV = (A.remoteView = A.remoteView || {})
  const $ = (id) => document.getElementById(id)

  RV.deviceId = ''
  RV.cams = []
  RV.mics = []
  RV._sfu = null // dev:view 返回的 LiveKit 连接信息
  RV._room = null // LiveKit Room
  RV._kind = ''
  RV._bound = false
  RV.cpuHist = []
  RV.memHist = []

  function log(msg) { console.log('[rv] ' + msg) }

  function gb(n) { return n == null ? '—' : (n / 1073741824).toFixed(2) + ' GB' }
  function mb(n) { return n == null ? '—' : (n / 1048576).toFixed(0) + ' MB' }
  function uptime(s) { if (!s) return '—'; const d = Math.floor(s / 86400); const h = Math.floor((s % 86400) / 3600); const m = Math.floor((s % 3600) / 60); return d + '天 ' + h + '时 ' + m + '分' }
  function drawLine(canvas, hist, color, max) {
    if (!canvas) return
    const g = canvas.getContext('2d')
    const w = canvas.width, h = canvas.height
    g.clearRect(0, 0, w, h)
    g.strokeStyle = '#1f2430'; g.lineWidth = 1
    g.strokeRect(0, 0, w - 1, h - 1)
    const n = hist.length
    if (n < 2) return
    g.strokeStyle = color; g.lineWidth = 2
    g.beginPath()
    for (let i = 0; i < n; i++) {
      const x = (i / (n - 1)) * (w - 2) + 1
      const y = h - 1 - (hist[i] / max) * (h - 4)
      if (i === 0) g.moveTo(x, y); else g.lineTo(x, y)
    }
    g.stroke()
  }
  // 圆环图：used/total 占比，中心显示百分比
  function drawPie(canvas, used, total, color) {
    if (!canvas) return
    const g = canvas.getContext('2d')
    const w = canvas.width, h = canvas.height, cx = w / 2, cy = h / 2
    const r = Math.min(w, h) / 2 - 10
    const pct = total > 0 ? Math.min(100, Math.max(0, (used / total) * 100)) : 0
    g.clearRect(0, 0, w, h)
    g.lineWidth = 14
    g.strokeStyle = '#232936'
    g.beginPath(); g.arc(cx, cy, r, 0, Math.PI * 2); g.stroke()
    if (pct > 0) {
      g.strokeStyle = color
      g.beginPath(); g.arc(cx, cy, r, -Math.PI / 2, -Math.PI / 2 + (pct / 100) * Math.PI * 2); g.stroke()
    }
    g.fillStyle = '#e6eaf2'
    g.font = 'bold 22px sans-serif'
    g.textAlign = 'center'; g.textBaseline = 'middle'
    g.fillText(Math.round(pct) + '%', cx, cy)
  }
  function pieTip(part, total, label) {
    const usedGb = (part || 0) / 1073741824, totalGb = (total || 0) / 1073741824
    const pct = total > 0 ? Math.round((part / total) * 100) : 0
    return label + '：已用 ' + usedGb.toFixed(1) + 'GB / 共 ' + totalGb.toFixed(1) + 'GB（' + pct + '%），剩余 ' + ((totalGb - usedGb) || 0).toFixed(1) + 'GB'
  }
  // 磁盘：每个分区/硬盘单独画一个圆饼（多盘 → 多列自适应，flex-wrap 自动换行）
  function renderDisks(fs, info) {
    const wrap = document.getElementById('pie-disks')
    if (!wrap) return
    wrap.innerHTML = ''
    if (!fs || !fs.length) { wrap.innerHTML = '<div class="pie-nodata">' + (info.disk || '等待上报磁盘信息…') + '</div>'; return }
    for (const f of fs) {
      const item = document.createElement('div'); item.className = 'pie-item'
      const c = document.createElement('canvas'); c.width = 120; c.height = 120
      item.appendChild(c)
      const t = document.createElement('div'); t.className = 'pie-item-tip'
      const usedG = Math.round((f.used || 0) / 1073741824), sizeG = Math.round((f.size || 0) / 1073741824)
      t.textContent = (f.mount || '盘') + ' ' + usedG + '/' + sizeG + 'GB' + (f.use != null ? ' · ' + Math.round(f.use) + '%' : '')
      item.appendChild(t)
      wrap.appendChild(item)
      drawPie(c, f.used || 0, f.size || 0, '#f0a14a')
    }
  }

  // ── 远程系统信息（客户端 dev:sys 上报）──
  function fmtMem(mem) { return (mem || []).map((m) => (m.capacity || '') + ' · ' + (m.speed || '') + ' · ' + (m.manufacturer || '') + ' · ' + (m.type || '')).join('\n') || '—' }
  function fmtGpuInfo(gpus) { return (gpus || []).map((g) => (g.name || '') + (g.vram ? ' 显存:' + g.vram : '') + (g.driver ? ' 驱动:' + g.driver : '')).join('\n') || '—' }
  function fmtDevices(devs) { return (devs || []).map((d) => '[' + (d.type || '设备') + '] ' + (d.name || '') + (d.mac ? ' MAC:' + d.mac : '') + (d.size ? ' 容量:' + d.size : '')).join('\n') || '—' }
  function fmtProcs(procs) { return (procs || []).map((p) => (p.name || '') + '  PID:' + (p.pid || '') + '  内存:' + (p.mem != null ? p.mem + 'MB' : '—') + '  CPU:' + (p.cpu != null ? p.cpu + 's' : '—')).join('\n') || '—' }
  RV._renderSys = function (info) {
    if (!info) return
    function setTxt(id, val) { const el = document.getElementById(id); if (el) el.textContent = val }
    setTxt('s-host', info.hostname || '—')
    const ips = (info.ips || []).filter((i) => !i.internal).map((i) => i.address + ' (' + i.name + ')')
    setTxt('s-ip', ips.length ? ips.join('，') : ((info.ips || []).map((i) => i.address).join('，') || '—'))
    setTxt('s-os', (info.type || '') + ' ' + (info.release || '') + ' ' + (info.arch || ''))
    setTxt('s-cpu', (info.cpuModel || '—') + ' · ' + (info.cpuCores || 0) + ' 核')
    setTxt('s-mem', gb(info.totalMem))
    setTxt('s-board', info.board || '—')
    setTxt('s-gpu', (info.gpuInfo || []).map((g) => g.name || '').filter(Boolean).join(' / ') || info.gpu || '—')
    setTxt('s-uptime', uptime(info.uptime))
    // 磁盘：每个盘单独一个圆饼
    renderDisks(info.fs || [], info)
    setTxt('s-disk', info.disk || '—')
    // 内存圆饼
    drawPie(document.getElementById('pie-mem'), (info.totalMem || 0) - (info.freeMem || 0), info.totalMem, '#00c88a')
    setTxt('mem-tip', pieTip((info.totalMem || 0) - (info.freeMem || 0), info.totalMem, '内存'))
    setTxt('s-memdetail', fmtMem(info.memDetail))
    setTxt('s-gpuinfo', fmtGpuInfo(info.gpuInfo))
    setTxt('s-devices', fmtDevices(info.devices))
    setTxt('s-procs', fmtProcs(info.processes))
  }
  RV._renderPerf = function (perf) {
    if (!perf) return
    function setTxt(id, val) { const el = document.getElementById(id); if (el) el.textContent = val }
    if (perf.cpu != null) { RV.cpuHist.push(perf.cpu); if (RV.cpuHist.length > 80) RV.cpuHist.shift() }
    if (perf.memPercent != null) { RV.memHist.push(perf.memPercent); if (RV.memHist.length > 80) RV.memHist.shift() }
    setTxt('perf-cpu-v', (perf.cpu != null ? perf.cpu + '%' : '—'))
    setTxt('perf-mem-v', (perf.memPercent != null ? perf.memPercent + '%' : '—') + (perf.memUsed != null && perf.memTotal != null ? '（' + mb(perf.memUsed) + ' / ' + gb(perf.memTotal) + '）' : ''))
    if (perf.memUsed != null && perf.memTotal != null) {
      drawPie(document.getElementById('pie-mem'), perf.memUsed, perf.memTotal, '#00c88a')
      setTxt('mem-tip', pieTip(perf.memUsed, perf.memTotal, '内存'))
    }
    drawLine(document.getElementById('perf-cpu'), RV.cpuHist, '#4a6cf7', 100)
    drawLine(document.getElementById('perf-mem'), RV.memHist, '#00c88a', 100)
    if (perf.gpus) setTxt('s-gpuload', perf.gpus.map((g) => (g.name || 'GPU') + ' : ' + (g.load != null ? g.load + '%' : '—')).join('\n') || '—')
  }

  function bind() {
    if (RV._bound) return
    RV._bound = true
    A.sock.on('dev:devices', (d) => {
      if (!RV.deviceId || d.deviceId !== RV.deviceId) return
      RV.cams = d.cams || []
      RV.mics = d.mics || []
      log('收到设备列表 cams=' + RV.cams.length + ' mics=' + RV.mics.length)
    })
    A.sock.on('dev:sys', (d) => {
      if (!RV.deviceId || d.deviceId !== RV.deviceId) return
      RV._renderSys(d.info)
      RV._renderPerf(d.perf)
    })
    A.sock.on('dev:signal', (d) => {
      if (!RV.deviceId || d.deviceId !== RV.deviceId) return
      RV._handleSignal(d.signal || {})
    })
    A.sock.on('dev:start', () => {})
    // 服务端广播的设备在线状态变化。判定来源只有一处：控制端/被控端 socket 断开——
    // 被控端进程被强杀时 OS 会关闭 fd 发出 FIN，服务端读循环立刻报错，随即广播 dev:status。
    A.sock.on('dev:status', (d) => {
      if (!d || !RV.deviceId || d.deviceId !== RV.deviceId) return
      if (d.online !== false) return
      log('收到设备离线广播 reason=' + (d.reason || ''))
      RV.handleDeviceOffline(d.reason)
    })
    // 注意：这是「浏览器 ↔ 服务端」的 socket 断开，不是被控设备断开（别把两者混为一谈）。
    // 断开后收不到任何推送，画面会停在最后一帧，所以先把远程查看收尾。
    A.sock.on('disconnect', () => {
      log('与服务端 socket 断开（非设备离线）→ 停止远程查看')
      RV.stop()
    })
  }

  // handleDeviceOffline 被控端设备离线：停止远程查看 + 提示 + 退回设备列表。
  RV.handleDeviceOffline = function (reason) {
    RV.stop()
    const msg = reason === 'removed'
      ? '远程设备已离线（服务端已清理该设备）'
      : '远程设备已离线（连接已断开：被强制退出或掉线）'
    A.toast(msg)
    const st = $('rv-state')
    if (st) st.textContent = msg
    // 只在详情页仍显示着该设备时才点「返回」，避免误触
    const detail = $('dev-detail')
    const back = $('dd-back')
    if (back && detail && detail.style.display !== 'none') back.click()
  }

  RV.open = async function (device) {
    RV.deviceId = device.id
    RV.cams = []
    RV.mics = []
    RV._kind = ''
    RV._pendCands = []
    bind()
    $('remote-panel').style.display = ''
    $('rv-empty').style.display = ''
    $('rv-state').textContent = '连接中…'
    $('rv-video').srcObject = null
    $('rv-cam').disabled = false
    $('rv-screen').disabled = false
    $('rv-mic').disabled = false
    $('rv-stop').disabled = false
    const ack = await A.emit('dev:view', { deviceId: RV.deviceId }).catch(() => ({ ok: false }))
    if (!ack.ok) { $('rv-state').textContent = '查看失败：' + (ack.error || ''); return }
    RV._sfu = ack.sfu || null // 服务端为控制端/被控端签发同一 LiveKit room 的 token
    $('rv-state').textContent = ack.name + '（' + ack.ip + '）在线'
    // 初始系统信息快照（dev:view 随 ack 返回的缓存）；无缓存时先用 ack 基础信息填充，
    // 避免残留服务器本机的旧值，等待 dev:sys 实时上报补齐
    RV.cpuHist = []
    RV.memHist = []
    if (ack.sys) {
      RV._renderSys(ack.sys.info)
      RV._renderPerf(ack.sys.perf)
    } else {
      RV._renderSys({
        hostname: ack.name,
        os: ack.os,
        ips: ack.ip ? [{ name: '', address: ack.ip, internal: false }] : [],
        cpuModel: undefined, cpuCores: 0, totalMem: undefined, board: '', gpu: '', uptime: undefined, disk: '等待客户端上报系统信息…'
      })
      RV._renderPerf(null)
      const setTxt = (id, val) => { const el = document.getElementById(id); if (el) el.textContent = val }
      setTxt('perf-cpu-v', '—')
      setTxt('perf-mem-v', '—')
      setTxt('s-disk', '等待客户端上报系统信息…')
    }
    A.emit('dev:enumerate', { deviceId: RV.deviceId })
  }

  // ─── SFU 连接：控制端连 LiveKit room，订阅被控端发布的屏幕/摄像头/麦克风轨道 ───
  RV._connectSfu = async function () {
    if (RV._room) return RV._room
    if (!RV._sfu || !RV._sfu.url || !RV._sfu.token) {
      $('rv-state').textContent = '查看失败：未获得 SFU 连接信息（服务端未启用 LiveKit）'
      return null
    }
    const LK = window.LivekitClient
    const room = new LK.Room()
    RV._room = room
    room.on(LK.RoomEvent.TrackSubscribed, (track, _pub, _participant) => {
      const mt = track.mediaStreamTrack
      if (!mt) return
      const vid = $('rv-video')
      // 被控端每条轨道单独发布；切 kind 时被控端 publish/replace，这里取最新轨道重建流
      vid.srcObject = new MediaStream([mt])
      vid.muted = RV._kind !== 'mic' // 摄像头静音；麦克风放音
      void vid.play().catch(() => {})
      $('rv-empty').style.display = 'none'
      $('rv-state').textContent = '正在查看' + (RV._kind === 'screen' ? '屏幕' : RV._kind === 'mic' ? '麦克风' : '摄像头')
    })
    room.on(LK.RoomEvent.ParticipantDisconnected, () => { RV._closeRoom() })
    try {
      await room.connect(RV._sfu.url, RV._sfu.token)
    } catch (e) {
      try { room.disconnect() } catch {}
      RV._room = null
      $('rv-state').textContent = 'SFU 连接失败：' + (e instanceof Error ? e.message : String(e))
      return null
    }
    return room
  }
  RV._closeRoom = function () {
    if (RV._room) {
      try { RV._room.disconnect() } catch {}
      RV._room = null
    }
  }

  RV.start = async function (kind, device) {
    if (!RV.deviceId) { A.toast('请先在上方设备列表里选择一台在线电脑'); return }
    RV._kind = kind
    $('rv-empty').style.display = ''
    $('rv-state').textContent = '正在请求' + (kind === 'screen' ? '屏幕' : kind === 'mic' ? '麦克风' : '摄像头') + '…'
    const ack = await A.emit('dev:start', { deviceId: RV.deviceId, kind, device }).catch(() => ({ ok: false }))
    if (!ack.ok) { $('rv-state').textContent = '启动失败：' + (ack.error || ''); return }
    // 连接 LiveKit room（首次建立；已连接则复用，被控端 replaceTrack/publish 新轨后 TrackSubscribed 更新画面）
    const room = await RV._connectSfu()
    if (!room) return
    log('SFU 已连接，等待被控端发布 ' + kind + ' 轨道…')
  }

  RV.cam = function () { RV._menu(RV.cams, 'camera', '选择摄像头') }
  RV.mic = function () { RV._menu(RV.mics, 'mic', '选择麦克风') }
  RV.screen = function () { RV.start('screen') }

  // 设备按钮（麦克风/摄像头）的统一交互，管理页与独立窗口共用：
  //   单击 / 点按 → 直接用「默认设备」开始（不再先弹列表，点了就有画面/声音）
  //   右键        → 弹出设备列表，手动指定设备
  //   长按        → 弹出设备列表（移动端没有右键；iOS 长按不会触发 contextmenu，必须自己计时）
  RV.bindDeviceButtons = function () {
    const defs = [
      { id: 'rv-mic', kind: 'mic', title: '选择麦克风', list: () => RV.mics },
      { id: 'rv-cam', kind: 'camera', title: '选择摄像头', list: () => RV.cams }
    ]
    defs.forEach((d) => {
      const btn = $(d.id)
      if (!btn || btn.dataset.rvWire === '1') return // 幂等：重复 init 不重复绑定
      btn.dataset.rvWire = '1'
      const openMenu = () => RV._menu(d.list(), d.kind, d.title)

      let pressTimer = null
      let longPressed = false
      const LONG_PRESS_MS = 500

      btn.addEventListener('click', (e) => {
        e.preventDefault()
        // 长按结束后浏览器还会补发一次 click，这里吞掉，避免“长按选择设备”又立刻改用默认设备
        if (longPressed) { longPressed = false; return }
        RV.start(d.kind)
      })
      btn.addEventListener('contextmenu', (e) => { e.preventDefault(); openMenu() })

      const cancelPress = () => { clearTimeout(pressTimer) }
      btn.addEventListener('touchstart', () => {
        longPressed = false
        cancelPress()
        pressTimer = setTimeout(() => { longPressed = true; openMenu() }, LONG_PRESS_MS)
      }, { passive: true })
      btn.addEventListener('touchend', cancelPress)
      btn.addEventListener('touchcancel', cancelPress)
      btn.addEventListener('touchmove', cancelPress, { passive: true }) // 手指滑动（滚动页面）不算长按
    })
  }

  // 右键设备菜单（摄像头/麦克风列表）
  RV._menu = function (list, kind, title) {
    const m = $('rv-menu')
    m.innerHTML = ''
    const t = document.createElement('div'); t.className = 'rv-menu-title'; t.textContent = title; m.appendChild(t)
    if (!list || !list.length) {
      const e = document.createElement('div'); e.className = 'rv-menu-item'; e.textContent = '（暂无设备，请先在目标机连接设备）'; m.appendChild(e)
    } else {
      list.forEach((dev) => {
        const it = document.createElement('div'); it.className = 'rv-menu-item'; it.textContent = dev.label || dev.id
        it.onclick = () => { RV.start(kind, dev.id); RV.hideMenu() }
        m.appendChild(it)
      })
    }
    m.style.display = ''
    // 定位到触发按钮上方
    const btn = kind === 'mic' ? $('rv-mic') : $('rv-cam')
    const r = btn.getBoundingClientRect()
    const menuH = m.offsetHeight || 180
    const top = Math.max(8, r.top - menuH - 4)
    m.style.left = Math.max(8, Math.min(r.left, window.innerWidth - 240)) + 'px'
    m.style.top = top + 'px'
  }
  RV.hideMenu = function () { $('rv-menu').style.display = 'none' }

  // 独立前置窗口（画中画 PiP）：把远程视频弹到系统级置顶小窗，独立于页面
  RV.pip = function () {
    const vid = $('rv-video')
    if (!vid || !document.pictureInPictureEnabled) { alert('当前浏览器不支持画中画（建议 Chrome / Edge）'); return }
    if (document.pictureInPictureElement === vid) {
      void document.exitPictureInPicture().catch(() => {})
    } else if (vid.readyState >= 1 && vid.srcObject) {
      void vid.requestPictureInPicture().catch((e) => log('PiP 失败 ' + e))
    } else {
      alert('请先开始远程查看（共享屏幕/摄像头）')
    }
  }
  function pipState() {
    const btn = document.getElementById('rv-pip')
    if (!btn) return
    const active = document.pictureInPictureElement
    btn.classList.toggle('rv-ctrl-on', !!active)
    btn.title = active ? '退出独立窗口（回到内嵌画面）' : '独立前置窗口（画中画，置顶显示远程画面）'
  }
  if (typeof document.pictureInPictureEnabled === 'boolean') {
    document.addEventListener('enterpictureinpicture', pipState)
    document.addEventListener('leavepictureinpicture', pipState)
  }

  RV.stop = function () {
    if (document.pictureInPictureElement) { try { void document.exitPictureInPicture() } catch {} }
    if (RV.deviceId) A.emit('dev:stop', { deviceId: RV.deviceId }).catch(() => {})
    RV._closeRoom()
    RV._kind = ''
    const vid = $('rv-video')
    if (vid) vid.srcObject = null
    RV.deviceId = ''
  }
})()
