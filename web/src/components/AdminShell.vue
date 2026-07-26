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
  <div class="dashboard-shell">
    <a class="skip-link" href="#main-content">跳到主要内容</a>
    <header class="topbar">
      <RouterLink class="wordmark" to="/" aria-label="Vitals 运营总览">
        <span class="wordmark-pulse" aria-hidden="true" />
        <span>Vitals</span>
      </RouterLink>
      <nav class="grouped-nav" aria-label="主导航">
        <div class="nav-group">
          <span class="nav-group-label">监控</span>
          <RouterLink to="/">
            运营总览
          </RouterLink>
          <RouterLink to="/monitor/accounts">
            账号体征
          </RouterLink>
        </div>
        <div class="nav-group">
          <span class="nav-group-label">分配</span>
          <RouterLink to="/allocation/accounts">
            账号池
          </RouterLink>
          <RouterLink to="/allocation/cards">
            卡密
          </RouterLink>
          <RouterLink to="/allocation/allocations">
            分配记录
          </RouterLink>
        </div>
        <div class="nav-group">
          <RouterLink to="/settings">
            配置
          </RouterLink>
        </div>
      </nav>
      <button class="logout-button" type="button" @click="logout">
        退出
      </button>
    </header>
    <main id="main-content" class="dashboard-main">
      <slot />
    </main>
    <footer class="dashboard-footer">
      <span>VITALS / 统一运营后台</span><span>本机回环 · 所有时间均按 UTC 解释</span>
    </footer>
  </div>
</template>
