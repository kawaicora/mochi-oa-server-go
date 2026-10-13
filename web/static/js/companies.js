;(function () {
  const A = window.__admin
  if (!A) return
  const $ = (id) => document.getElementById(id)
  const C = (A.companies = A.companies || {})
  C.list = []

  const ROLE_ZH = { owner: '所有者', admin: '管理员', member: '成员' }

  C.load = async function () {
    $('companies-grid').innerHTML = '<div class="empty">加载中…</div>'
    const ack = await A.emit('admin:listCompanies', {}).catch(() => ({ ok: false }))
    if (!ack.ok) { $('companies-grid').innerHTML = '<div class="empty">' + A.esc(ack.error || '获取公司失败') + '</div>'; return }
    C.list = ack.companies || []
    $('companies-count').textContent = C.list.length
    C.render()
  }

  C.render = function () {
    const grid = $('companies-grid')
    if (!C.list.length) { grid.innerHTML = '<div class="empty">暂无公司</div>'; return }
    grid.innerHTML = ''
    C.list.forEach((co) => {
      const card = document.createElement('div')
      card.className = 'company-card'
      const counts = {}
      ;(co.members || []).forEach((m) => { counts[m.role] = (counts[m.role] || 0) + 1 })
      const countChips = Object.keys(counts).map((r) => '<span class="chip">' + (ROLE_ZH[r] || r) + ' ' + counts[r] + '</span>').join('')
      const mems = (co.members || []).map((m) =>
        '<div class="cm-row"><span class="cm-avatar">' + (m.avatar ? '<img src="' + A.esc(m.avatar) + '" onerror="this.style.display=\'none\'">' : A.esc((m.nick || m.username || '?').charAt(0))) + '</span><span class="cm-name">' + A.esc(m.nick || m.username) + '</span><span class="cm-role">' + (ROLE_ZH[m.role] || m.role) + '</span></div>'
      ).join('')
      card.innerHTML =
        '<div class="co-head">' +
        '<div class="co-name">' + A.esc(co.name) + (co.id ? ' <span class="co-id">#' + co.id + '</span>' : '') + '</div>' +
        '<div class="co-counts">' + countChips + '</div>' +
        '</div>' +
        '<div class="co-members">' + (mems || '<div class="empty">暂无成员</div>') + '</div>'
      grid.appendChild(card)
    })
  }

  C.init = function () {
    if (C._inited) return
    C._inited = true
    C.load()
    $('companies-refresh').onclick = C.load
  }
})()
