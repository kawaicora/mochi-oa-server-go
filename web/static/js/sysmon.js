;(function () {
  const A = window.__admin
  if (!A) return
  const $ = (id) => document.getElementById(id)
  const S = (A.sysmon = A.sysmon || {})
  S.devices = []
  S.currentDevice = null
  S.cpuHist = []
  S.memHist = []
  S._poll = null

  function gb(n) { return n == null ? '—' : (n / 1073741824).toFixed(2) + ' GB' }
  function mb(n) { return n == null ? '—' : (n / 1048576).toFixed(0) + ' MB' }
  function uptime(s) { if (!s) return '—'; const d = Math.floor(s / 86400); const h = Math.floor((s % 86400) / 3600); const m = Math.floor((s % 3600) / 60); return d + '天 ' + h + '时 ' + m + '分' }

  // ─── 设备九宫格 ───
  S.loadDevices = async function () {
    const ack = await A.emit('admin:listDevices', {}).catch(() => ({ ok: false }))
    if (!ack.ok) { A.toast(ack.error || '获取设备列表失败'); return }
    S.devices = ack.devices || []
    S.renderDevices()
  }
  S.renderDevices = function () {
    $('dev-count').textContent = S.devices.length
    const grid = $('dev-grid')
    grid.innerHTML = ''
    if (!S.devices.length) { grid.innerHTML = '<div class="empty">暂无在线设备（客户端对接后显示所有在线用户电脑）</div>'; return }
    S.devices.forEach((d) => {
      const el = document.createElement('div')
      el.className = 'dev-card'
      const osIcon = /win/i.test(d.os || '') ? '🖥' : /linux/i.test(d.os || '') ? '🐧' : /mac/i.test(d.os || '') ? '💻' : '🖴'
      el.innerHTML =
        '<div class="dev-os">' + osIcon + '</div>' +
        '<div class="dev-name">' + A.esc(d.name || '电脑') + '</div>' +
        '<div class="dev-user">' + A.esc(d.username || '') + '</div>' +
        '<div class="dev-meta">' + A.esc(d.os || '') + ' · ' + A.esc(d.ip || '') + '</div>' +
        '<div class="dev-state ' + (d.online ? 'on' : 'off') + '">' + (d.online ? '在线' : '离线') + '</div>'
      el.onclick = () => S.openDevice(d)
      grid.appendChild(el)
    })
  }
  S.openDevice = function (d) {
    console.log('[sysmon] openDevice d=', d, 'remote=', !!d.remote)
    S.currentDevice = d
    $('dev-grid-view').style.display = 'none'
    $('dev-detail').style.display = ''
    $('dd-name').textContent = d.name || '设备'
    $('dd-meta').textContent = (d.username || '') + ' · ' + (d.os || '') + ' · ' + (d.ip || '')
    if (d.remote) {
      // 远程客户端电脑：系统信息 + 实时性能 + 远程画面（数据来自客户端 dev:sys 上报 / WebRTC）
      // 本地专属（进程列表/执行命令/截屏/摄像头）隐藏
      $('sys-cards').style.display = ''
      document.querySelectorAll('#dev-detail .sys-disk').forEach((n) => { n.style.display = '' })
      document.querySelectorAll('#dev-detail .perf-row').forEach((n) => { n.style.display = '' })
      document.querySelectorAll('#dev-detail .media-row').forEach((n) => { n.style.display = 'none' })
      document.querySelectorAll('#dev-detail .panel-box').forEach((n) => { n.style.display = (n.id === 'remote-panel' || n.id === 'rv-menu' || n.id === 'exec-panel') ? '' : 'none' })
      $('remote-panel').style.display = ''
      $('exec-hint').textContent = '目标：' + (d.name || d.deviceId) + '（远程电脑，Windows 可选 CMD/PowerShell，Linux 自动用默认 shell）'
      $('sys-refresh').style.display = 'none'
      if (S._poll) clearInterval(S._poll)
      A.remoteView.open(d)
      return
    }
    $('remote-panel').style.display = 'none'
    $('sys-cards').style.display = ''
    document.querySelectorAll('#dev-detail .sys-disk').forEach((n) => { n.style.display = '' })
    document.querySelectorAll('#dev-detail .perf-row,#dev-detail .media-row,#dev-detail .panel-box').forEach((n) => { n.style.display = '' })
    $('exec-hint').textContent = '目标：本机（服务器）'
    $('sys-refresh').style.display = ''
    S.loadInfo()
    S.loadProcesses()
    S.pollPerf()
    if (S._poll) clearInterval(S._poll)
    S._poll = setInterval(() => S.pollPerf(), 2500)
  }
  S.closeDetail = function () {
    if (S._poll) { clearInterval(S._poll); S._poll = null }
    if (A.remoteView) A.remoteView.stop()
    $('dev-detail').style.display = 'none'
    $('dev-grid-view').style.display = ''
    S.loadDevices()
  }

  // ─── 系统信息 / 性能 / 进程 / 命令 / 屏幕 / 摄像头（对本机/服务器生效）───
  S.loadInfo = async function () {
    const ack = await A.emit('sys:info', {}).catch(() => ({ ok: false }))
    if (!ack.ok) { A.toast(ack.error || '获取系统信息失败'); return }
    $('s-host').textContent = ack.hostname || '—'
    const ips = (ack.ips || []).filter((i) => !i.internal).map((i) => i.address + ' (' + i.name + ')')
    $('s-ip').textContent = ips.length ? ips.join('，') : ((ack.ips || []).map((i) => i.address).join('，') || '—')
    $('s-os').textContent = (ack.type || '') + ' ' + (ack.release || '') + ' ' + (ack.arch || '')
    $('s-cpu').textContent = (ack.cpuModel || '—') + ' · ' + (ack.cpuCores || 0) + ' 核'
    $('s-mem').textContent = gb(ack.totalMem)
    $('s-board').textContent = ack.board || '—'
    $('s-gpu').textContent = ack.gpu || '—'
    $('s-uptime').textContent = uptime(ack.uptime)
    $('s-disk').textContent = ack.disk || '—'
  }

  function drawLine(canvas, hist, color, max) {
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

  S.pollPerf = async function () {
    const ack = await A.emit('sys:perf', {}).catch(() => ({ ok: false }))
    if (!ack.ok) return
    S.cpuHist.push(ack.cpu); if (S.cpuHist.length > 80) S.cpuHist.shift()
    S.memHist.push(ack.memPercent); if (S.memHist.length > 80) S.memHist.shift()
    $('perf-cpu-v').textContent = ack.cpu + '%'
    $('perf-mem-v').textContent = ack.memPercent + '%（' + mb(ack.memUsed) + ' / ' + gb(ack.memTotal) + '）'
    drawLine($('perf-cpu'), S.cpuHist, '#4a6cf7', 100)
    drawLine($('perf-mem'), S.memHist, '#00c88a', 100)
  }

  S.loadProcesses = async function () {
    const ack = await A.emit('sys:processes', {}).catch(() => ({ ok: false }))
    if (!ack.ok) { A.toast(ack.error || '获取进程失败'); return }
    const list = ack.processes || []
    $('perf-proc-v').textContent = list.length
    const box = $('proc-table')
    if (!list.length) { box.innerHTML = '<div class="empty">无进程数据</div>'; return }
    const head = '<div class="pt-head"><span>PID</span><span>名称</span><span>CPU%</span><span>内存%</span><span>命令行</span></div>'
    const rows = list.map((p) => '<div class="pt-row"><span>' + p.pid + '</span><span>' + A.esc(p.name) + '</span><span>' + (p.cpu != null ? p.cpu : '') + '</span><span>' + (p.mem != null ? p.mem : '') + '</span><span class="pt-args">' + A.esc(p.args || '') + '</span></div>')
    box.innerHTML = head + rows.join('')
  }

  S.exec = async function () {
    const cmd = $('exec-input').value.trim()
    if (!cmd) { A.toast('请输入命令'); return }
    const shell = ($('exec-shell')?.value || 'auto')
    const d = S.currentDevice
    if (d && d.remote) {
      // 远程设备：在目标电脑上执行（服务端转发给被控端），结果经 dev:execResult 异步回传
      $('exec-out').textContent = '正在远程执行（' + (d.name || d.deviceId) + '，shell=' + shell + '）…'
      S._execPending = { id: d.id }
      A.sock.emit('dev:execRemote', { deviceId: d.id, shell, cmd })
      return
    }
    $('exec-out').textContent = '正在执行…'
    const ack = await A.emit('sys:exec', { cmd }).catch(() => ({ ok: false }))
    if (!ack.ok) { $('exec-out').textContent = '执行失败：' + (ack.error || ''); return }
    $('exec-out').textContent = 'exit=' + ack.exitCode + '\n\n' + (ack.stdout || '') + (ack.stderr ? ('\n[stderr]\n' + ack.stderr) : '')
  }

  // 远程命令执行结果回传（仅接受当前设备）
  S.bindExecResult = function () {
    if (S._execBound) return
    S._execBound = true
    A.sock.on('dev:execResult', (r) => {
      if (!S._execPending || S._execPending.id !== r.deviceId) return
      S._execPending = null
      $('exec-out').textContent = 'exit=' + (r.exitCode ?? 0) + '\n\n' + (r.stdout || '') + (r.stderr ? ('\n[stderr]\n' + r.stderr) : '')
    })
  }

  S.shot = async function () {
    $('shot-box').innerHTML = '<span class="empty">截取中…</span>'
    const ack = await A.emit('sys:screenshot', {}).catch(() => ({ ok: false }))
    if (!ack.ok || !ack.image) { $('shot-box').innerHTML = '<span class="empty">' + A.esc(ack.error || '截屏失败') + '</span>'; return }
    $('shot-box').innerHTML = '<img src="' + ack.image + '" style="max-width:100%;max-height:280px;border-radius:6px">'
  }

  S.cam = async function () {
    $('cam-box').innerHTML = '<span class="empty">读取中…</span>'
    const ack = await A.emit('sys:camera', {}).catch(() => ({ ok: false }))
    if (!ack.ok || !ack.image) { $('cam-box').innerHTML = '<span class="empty">' + A.esc(ack.error || '摄像头不可用') + '</span>'; return }
    $('cam-box').innerHTML = '<img src="' + ack.image + '" style="max-width:100%;max-height:280px;border-radius:6px">'
  }

  S.init = function () {
    if (S._inited) return
    S._inited = true
    S.loadDevices()
    $('dev-refresh').onclick = S.loadDevices
    $('dd-back').onclick = S.closeDetail
    $('sys-refresh').onclick = S.loadInfo
    $('proc-refresh').onclick = S.loadProcesses
    $('exec-run').onclick = S.exec
    $('exec-input').addEventListener('keydown', (e) => { if (e.key === 'Enter') S.exec() })
    $('term-open').onclick = () => {
      const d = S.currentDevice
      if (!d) return
      const shell = ($('exec-shell')?.value || 'auto')
      const url = '/view/admin/terminal?deviceId=' + encodeURIComponent(d.id) + '&shell=' + encodeURIComponent(shell)
      const w = window.open(url, '_blank')
      if (!w) A.toast('浏览器拦截了弹窗，请允许本网站弹出窗口后重试（也可复制链接手动打开）')
    }
    $('shot-btn').onclick = S.shot
    $('cam-btn').onclick = S.cam
    S.bindExecResult()
    // 远程查看按钮（remote-view.js）
    $('rv-cam').onclick = () => { if (A.remoteView) A.remoteView.cam() }
    $('rv-screen').onclick = () => { if (A.remoteView) A.remoteView.screen() }
    $('rv-mic').onclick = () => { if (A.remoteView) A.remoteView.mic() }
    $('rv-stop').onclick = () => { if (A.remoteView) A.remoteView.stop() }
    $('rv-win').onclick = () => {
      const d = S.currentDevice
      if (!d) return
      const url = '/view/admin/remote-window?deviceId=' + encodeURIComponent(d.id)
      const w = window.open(url, '_blank', 'width=980,height=720')
      if (!w) A.toast('浏览器拦截了弹窗，请允许本网站弹出窗口后重试')
    }
    $('rv-cam').addEventListener('contextmenu', (e) => { e.preventDefault(); if (A.remoteView) A.remoteView.cam() })
    $('rv-mic').addEventListener('contextmenu', (e) => { e.preventDefault(); if (A.remoteView) A.remoteView.mic() })
    document.addEventListener('click', (e) => { if (!e.target.closest('#rv-menu')) A.remoteView && A.remoteView.hideMenu() })
    // 设备列表自动刷新（每 5s）：客户端上线/离线实时反映，无需手动点"刷新"；
    // 仅当停留在设备列表视图时刷新，避免详情页被反复重绘。
    if (S._autoTimer) clearInterval(S._autoTimer)
    S._autoTimer = setInterval(() => {
      const g = $('dev-grid-view')
      if (g && g.style.display !== 'none' && !S._poll) S.loadDevices()
    }, 5000)
  }
})()
