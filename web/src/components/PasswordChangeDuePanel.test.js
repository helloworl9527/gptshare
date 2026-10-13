import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '../api/client.js'
import { formatDateTime } from '../lib/vitals.js'
import PasswordChangeDuePanel from './PasswordChangeDuePanel.vue'

const fixture = {
  id: 12, display_username: 'needs-change@example.test', days_account_remains: 8,
  expired_card_count: 2, days_expired_card_expired_ago: 1,
  account_expiry: '2026-10-21T12:00:00Z', last_expired_card_expires_at: '2026-10-12T12:00:00Z',
}

function button(wrapper, name) {
  return wrapper.findAll('button').find((item) => item.text() === name)
}

describe('Password change reminder panel', () => {
  beforeEach(() => {
    vi.spyOn(api, 'passwordChangeDue').mockResolvedValue({ accounts: [fixture] })
    vi.spyOn(api, 'clearPasswordChange').mockResolvedValue({ id: 12, cleared: true })
  })

  it('shows the account, remaining time and expiry dates without credentials', async () => {
    const wrapper = mount(PasswordChangeDuePanel)
    expect(wrapper.text()).toContain('正在读取待更改密码清单')
    await flushPromises()
    expect(wrapper.text()).toContain('1 个账号')
    expect(wrapper.text()).toContain('needs-change@example.test')
    expect(wrapper.text()).toContain('账号剩余 8 天')
    expect(wrapper.text()).toContain('已过期卡密 2 张')
    expect(wrapper.text()).toContain('最近卡密已过期 1 天')
    expect(wrapper.text()).toContain(`账号到期：${formatDateTime(fixture.account_expiry)}`)
    expect(wrapper.text()).toContain(`最近卡密到期：${formatDateTime(fixture.last_expired_card_expires_at)}`)
    expect(wrapper.text()).toContain('不会修改密码或自动登出设备')
    expect(wrapper.find('input').exists()).toBe(false)
    wrapper.unmount()
  })

  it('requires confirmation, supports cancelling and removes only the marked account', async () => {
    api.passwordChangeDue.mockResolvedValue({ accounts: [fixture, { ...fixture, id: 13, display_username: 'other@example.test' }] })
    const wrapper = mount(PasswordChangeDuePanel)
    await flushPromises()
    await button(wrapper, '标记已更改密码').trigger('click')
    expect(api.clearPasswordChange).not.toHaveBeenCalled()
    expect(wrapper.get('[role="dialog"]').text()).toContain('needs-change@example.test')
    await button(wrapper, '取消').trigger('click')
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
    await button(wrapper, '标记已更改密码').trigger('click')
    await button(wrapper, '确认已处理').trigger('click')
    await flushPromises()
    expect(api.clearPasswordChange).toHaveBeenCalledExactlyOnceWith(12)
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
    expect(wrapper.findAll('.password-row')).toHaveLength(1)
    expect(wrapper.get('.password-row').text()).toContain('other@example.test')
    expect(wrapper.get('[role="status"]').text()).toContain('已记录 needs-change@example.test')
    wrapper.unmount()
  })

  it('shows an empty state after marking and can show a later reminder on refresh', async () => {
    const wrapper = mount(PasswordChangeDuePanel)
    await flushPromises()
    await button(wrapper, '标记已更改密码').trigger('click')
    await button(wrapper, '确认已处理').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('暂无待更改密码账号')
    await button(wrapper, '刷新密码清单').trigger('click')
    await flushPromises()
    expect(wrapper.findAll('.password-row')).toHaveLength(1)
    expect(wrapper.find('[role="status"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('keeps a failed mark in the list and allows retry in the dialog', async () => {
    api.clearPasswordChange.mockRejectedValueOnce(new Error('标记失败'))
    const wrapper = mount(PasswordChangeDuePanel)
    await flushPromises()
    await button(wrapper, '标记已更改密码').trigger('click')
    await button(wrapper, '确认已处理').trigger('click')
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toBe('标记失败')
    expect(wrapper.findAll('.password-row')).toHaveLength(1)
    await button(wrapper, '确认已处理').trigger('click')
    await flushPromises()
    expect(api.clearPasswordChange).toHaveBeenCalledTimes(2)
    expect(wrapper.text()).toContain('暂无待更改密码账号')
    wrapper.unmount()
  })

  it('blocks duplicate submits, refresh and modal dismissal while saving', async () => {
    let resolveMark
    api.clearPasswordChange.mockImplementation(() => new Promise((resolve) => { resolveMark = resolve }))
    const wrapper = mount(PasswordChangeDuePanel)
    await flushPromises()
    await button(wrapper, '标记已更改密码').trigger('click')
    await button(wrapper, '确认已处理').trigger('click')
    expect(button(wrapper, '正在记录…').element.disabled).toBe(true)
    expect(button(wrapper, '刷新密码清单').element.disabled).toBe(true)
    await wrapper.get('[aria-label="关闭弹窗"]').trigger('click')
    expect(wrapper.find('[role="dialog"]').exists()).toBe(true)
    await wrapper.vm.reload()
    expect(api.passwordChangeDue).toHaveBeenCalledOnce()
    resolveMark({ cleared: true })
    await flushPromises()
    expect(api.clearPasswordChange).toHaveBeenCalledOnce()
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('shows a local read error and retries without leaving stale entries actionable', async () => {
    api.passwordChangeDue.mockRejectedValueOnce(new Error('清单暂时不可用'))
    const wrapper = mount(PasswordChangeDuePanel)
    await flushPromises()
    expect(wrapper.text()).toContain('密码清单读取失败')
    expect(wrapper.text()).toContain('清单暂时不可用')
    await button(wrapper, '重试').trigger('click')
    await flushPromises()
    expect(wrapper.findAll('.password-row')).toHaveLength(1)
    api.passwordChangeDue.mockRejectedValueOnce(new Error('读取失败'))
    await button(wrapper, '刷新密码清单').trigger('click')
    await flushPromises()
    expect(wrapper.findAll('.password-row')).toHaveLength(0)
    expect(button(wrapper, '标记已更改密码')).toBeUndefined()
    wrapper.unmount()
  })

  it('ignores an older response when a newer refresh completes first', async () => {
    let resolveOld
    api.passwordChangeDue.mockImplementationOnce(() => new Promise((resolve) => { resolveOld = resolve }))
      .mockResolvedValueOnce({ accounts: [] })
    const wrapper = mount(PasswordChangeDuePanel)
    await wrapper.vm.reload()
    await flushPromises()
    resolveOld({ accounts: [fixture] })
    await flushPromises()
    expect(wrapper.text()).toContain('暂无待更改密码账号')
    expect(wrapper.findAll('.password-row')).toHaveLength(0)
    wrapper.unmount()
  })
})
