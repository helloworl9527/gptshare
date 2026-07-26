<script setup>
import { useRouter } from 'vue-router'
import { api, clearClientState } from '../api/client.js'

const router = useRouter()

async function logout() {
  try { await api.logout() } catch { /* local state is cleared either way */ }
  clearClientState()
  await router.replace('/login')
}
</script>

<template>
  <div class="admin-shell">
    <a class="skip-link" href="#main-content">跳到主要内容</a>
    <header class="topbar">
      <RouterLink class="wordmark" to="/" aria-label="Vitals 管理后台">
        <span class="wordmark-pulse" aria-hidden="true" />
        <span>Vitals</span>
      </RouterLink>
      <nav aria-label="管理员后台导航">
        <RouterLink to="/">
          Dashboard
        </RouterLink>
        <RouterLink to="/accounts">
          Accounts
        </RouterLink>
        <RouterLink to="/cards">
          Cards
        </RouterLink>
        <RouterLink to="/allocations">
          Allocations
        </RouterLink>
      </nav>
      <button class="logout-button" type="button" @click="logout">
        退出
      </button>
    </header>
    <main id="main-content" class="dashboard-main">
      <slot />
    </main>
    <footer class="dashboard-footer">
      <span>ALLOCATION VITALS / PHASE 02</span><span>本机回环管理后台</span>
    </footer>
  </div>
</template>
