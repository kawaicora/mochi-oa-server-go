;(function () {
  const A = window.__admin
  if (!A) return
  const $ = (id) => document.getElementById(id)
  const U = (A.users = A.users || {})
  U.list = []
  U.editing = null

  U.load = async function () {
    $('user-table').innerHTML = '<div class="empty">加载中…</div>'
    const ack = await A.emit('admin:listUsers', {}).catch(() => ({ ok: false }))
    if (!ack.ok) { $('user-table').innerHTML = '<div class="empty">' + A.esc(ack.error || '获取用户失败') + '</div>'; return }
    U.list = ack.users || []
    $('users-count').textContent = U.list.length
    U.render()
  }

  U.render = function () {
    const box = $('user-table')
    if (!U.list.length) { box.innerHTML = '<div class="empty">暂无用户</div>'; return }
    const head = '<div class="ut-head"><span>ID</span><span>用户名</span><span>昵称</span><span>头像</span><span>手机</span><span>邮箱</span><span>服务器管理员</span><span>所属公司</span><span>操作</span></div>'
    const rows = U.list.map((u) => {
      const cs = (u.companies || []).map((c) => c.name + '(' + c.role + ')').join('，') || '—'
      const av = u.avatar ? '<img src="' + A.esc(u.avatar) + '" style="width:28px;height:28px;border-radius:50%;object-fit:cover" onerror="this.style.display=\'none\'">' : ''
      const saLabel = u.serverAdmin ? '管理员' : '普通'
      const saBtn = u.serverAdmin
        ? '<button class="mini-btn" data-sa="' + u.username + '" data-sa-on="0">取消管理员</button>'
        : '<button class="mini-btn primary" data-sa="' + u.username + '" data-sa-on="1">设为管理员</button>'
      return (
        '<div class="ut-row">' +
        '<span>' + u.id + '</span>' +
        '<span>' + A.esc(u.username) + '</span>' +
        '<span>' + A.esc(u.nick || '') + '</span>' +
        '<span>' + av + '</span>' +
        '<span>' + A.esc(u.phone || '') + '</span>' +
        '<span>' + A.esc(u.email || '') + '</span>' +
        '<span class="sa-badge ' + (u.serverAdmin ? 'sa-on' : '') + '">' + saLabel + '</span>' +
        '<span>' + A.esc(cs) + '</span>' +
        '<span style="display:flex;gap:6px"><button class="mini-btn" data-edit="' + u.id + '">编辑</button>' + saBtn + '</span>' +
        '</div>'
      )
    })
    box.innerHTML = head + rows.join('')
    box.querySelectorAll('[data-edit]').forEach((b) => { b.onclick = () => U.openEdit(Number(b.getAttribute('data-edit'))) })
    box.querySelectorAll('[data-sa]').forEach((b) => { b.onclick = () => U.setServerAdmin(b.getAttribute('data-sa'), b.getAttribute('data-sa-on') === '1') })
  }

  U.setServerAdmin = async function (username, on) {
    const ack = await A.emit('admin:setServerAdmin', { username, admin: on }).catch(() => ({ ok: false }))
    if (!ack.ok) { A.toast(ack.error || '操作失败'); return }
    A.toast('已' + (on ? '设为' : '取消') + '服务器管理员：' + username)
    U.load()
  }

  U.openEdit = function (id) {
    const u = U.list.find((x) => x.id === id)
    if (!u) return
    U.editing = u
    $('ue-id').textContent = '#' + u.id
    $('ue-name').textContent = u.username + (u.nick ? '（' + u.nick + '）' : '')
    $('ue-nick').value = u.nick || ''
    $('ue-phone').value = u.phone || ''
    $('ue-email').value = u.email || ''
    $('ue-avatar').value = u.avatar || ''
    $('user-edit').style.display = ''
  }

  U.save = async function () {
    if (!U.editing) return
    const patch = {
      nick: $('ue-nick').value.trim(),
      phone: $('ue-phone').value.trim(),
      email: $('ue-email').value.trim(),
      avatar: $('ue-avatar').value.trim()
    }
    const ack = await A.emit('admin:updateUser', { userId: U.editing.id, patch }).catch(() => ({ ok: false }))
    if (!ack.ok) { A.toast(ack.error || '保存失败'); return }
    $('user-edit').style.display = 'none'
    U.editing = null
    A.toast('已保存')
    U.load()
  }

  U.init = function () {
    if (U._inited) return
    U._inited = true
    U.load()
    $('users-refresh').onclick = U.load
    $('ue-cancel').onclick = () => { $('user-edit').style.display = 'none'; U.editing = null }
    $('ue-save').onclick = U.save
  }
})()
