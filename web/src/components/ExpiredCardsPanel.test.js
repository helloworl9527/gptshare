import { flushPromises, mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import ExpiredCardsPanel from './ExpiredCardsPanel.vue'

function response(body, status = 200) {
  return { ok: status === 200, status, headers: new Headers(), json: async () => body }
}

const fixture = [{
  id: 10, code_suffix: 'ABCD', expires_at: '2026-10-07T12:00:00Z',
  accounts: [
    { id: 1, display_username: 'old@example.test', status: 'disabled', archived: true, active_card_count: 0, last_allocated_at: '2026-09-25T00:00:00Z' },
    { id: 2, display_username: 'shared@example.test', status: 'available', archived: false, active_card_count: 2, last_allocated_at: '2026-10-01T00:00:00Z' },
  ],
}]

describe('Expired card cleanup panel', () => {
  it('shows historical and archived accounts with warnings for current users', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response({ cards: fixture, total: 1, page: 1 })))
    const wrapper = mount(ExpiredCardsPanel)
    await flushPromises()
    expect(wrapper.text()).toContain('近 3 天已过期卡密及对应账号')
    expect(wrapper.text()).toContain('仅显示最近 72 小时内已过期的卡密')
    expect(wrapper.text()).toContain('**** ABCD')
    expect(wrapper.text()).toContain('卡密 #10')
    expect(wrapper.text()).toContain('old@example.test')
    expect(wrapper.text()).toContain('已下线')
    expect(wrapper.text()).toContain('shared@example.test')
    expect(wrapper.text()).toContain('仍有 2 张有效卡密')
    expect(wrapper.text()).toContain('无有效卡密')
    expect(wrapper.findAll('tbody tr')).toHaveLength(1)
    wrapper.unmount()
  })

  it('paginates and searches server-side, resetting the page for a new filter', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response({ cards: fixture, total: 21, page: 1 }))
      .mockResolvedValueOnce(response({ cards: [{ id: 11, code_suffix: 'EFGH', expires_at: '2026-10-01T00:00:00Z', accounts: [] }], total: 21, page: 2 }))
      .mockResolvedValueOnce(response({ cards: [], total: 0, page: 1 }))
    vi.stubGlobal('fetch', fetchMock)
    const wrapper = mount(ExpiredCardsPanel)
    await flushPromises()
    await wrapper.findAll('button').find((button) => button.text() === '下一页').trigger('click')
    await flushPromises()
    expect(String(fetchMock.mock.calls[1][0])).toContain('page=2')
    expect(wrapper.text()).toContain('无分配记录')
    await wrapper.find('input').setValue(' shared@example.test ')
    await wrapper.find('form').trigger('submit')
    await flushPromises()
    expect(String(fetchMock.mock.calls[2][0])).toContain('page=1')
    expect(String(fetchMock.mock.calls[2][0])).toContain('search=shared%40example.test')
    expect(wrapper.text()).toContain('没有匹配的过期卡密')
    wrapper.unmount()
  })

  it('retries a failed request and replaces the list after a renewed card disappears', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(response({}, 503))
      .mockResolvedValueOnce(response({ cards: fixture, total: 1, page: 1 }))
      .mockResolvedValueOnce(response({ cards: [], total: 0, page: 1 }))
    vi.stubGlobal('fetch', fetchMock)
    const wrapper = mount(ExpiredCardsPanel)
    await flushPromises()
    expect(wrapper.text()).toContain('过期清单读取失败')
    await wrapper.find('.error-panel button').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('shared@example.test')
    await wrapper.findAll('button').find((button) => button.text() === '刷新清单').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('暂无已过期卡密')
    expect(wrapper.text()).not.toContain('shared@example.test')
    wrapper.unmount()
  })
})
