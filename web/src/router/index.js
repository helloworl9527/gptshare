import { createRouter, createWebHistory } from 'vue-router'
import Login from '../views/Login.vue'
import Dashboard from '../views/Dashboard.vue'
import MonitorAccounts from '../views/MonitorAccounts.vue'
import AccountDetail from '../views/AccountDetail.vue'
import ImportWizard from '../views/ImportWizard.vue'
import Accounts from '../views/Accounts.vue'
import Cards from '../views/Cards.vue'
import Allocations from '../views/Allocations.vue'
import Settings from '../views/Settings.vue'
import { api, clearClientState } from '../api/client.js'

const router = createRouter({
  history: createWebHistory('/admin/'),
  routes: [
    { path: '/login', name: 'login', component: Login, meta: { public: true } },
    { path: '/', name: 'dashboard', component: Dashboard },
		{ path: '/monitor/accounts', name: 'monitor-accounts', component: MonitorAccounts },
		{ path: '/monitor/accounts/:id', name: 'account-detail', component: AccountDetail },
		{ path: '/monitor/import', name: 'import', component: ImportWizard },
		{ path: '/allocation/accounts', name: 'allocation-accounts', component: Accounts },
		{ path: '/allocation/cards', name: 'cards', component: Cards },
		{ path: '/allocation/allocations', name: 'allocations', component: Allocations },
    { path: '/settings', name: 'settings', component: Settings },
    { path: '/:pathMatch(.*)*', redirect: '/' },
  ],
  scrollBehavior: () => ({ top: 0 }),
})

router.beforeEach(async (to) => {
  if (to.meta.public) return true
  try {
    await api.me()
    return true
  } catch {
    clearClientState()
    return { name: 'login', query: { redirect: to.fullPath } }
  }
})

if (typeof window !== 'undefined') {
  window.addEventListener('session-expired', () => {
    if (router.currentRoute.value.name !== 'login') router.replace({ name: 'login', query: { reason: 'expired' } })
  })
}

export default router
