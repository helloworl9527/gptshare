import { createRouter, createWebHistory } from 'vue-router'
import Login from '../views/Login.vue'
import Dashboard from '../views/Dashboard.vue'
import Accounts from '../views/Accounts.vue'
import Cards from '../views/Cards.vue'
import Allocations from '../views/Allocations.vue'
import { api, clearClientState } from '../api/client.js'

const router = createRouter({
  history: createWebHistory(import.meta.env.BASE_URL),
  routes: [
    { path: '/login', name: 'login', component: Login, meta: { public: true } },
    { path: '/', name: 'dashboard', component: Dashboard },
    { path: '/accounts', name: 'accounts', component: Accounts },
    { path: '/cards', name: 'cards', component: Cards },
    { path: '/allocations', name: 'allocations', component: Allocations },
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
