<script setup>
import { computed } from 'vue'

const props = defineProps({ state: { type: String, required: true } })
const labels = {
  alive: '绿色搏动线：账号存活',
  near: '琥珀色搏动线：账号临期',
  suspect: '橙色搏动线：账号疑似被封禁，待人工确认',
  banned: '红色平直线：账号非正常失效',
  retired: '灰色虚线：账号正常到期',
}
const points = computed(() => props.state === 'banned' || props.state === 'retired'
  ? '0,26 240,26'
  : '0,26 54,26 68,26 78,7 92,44 106,17 118,26 164,26 174,26 184,14 196,36 208,26 240,26')
</script>

<template>
  <svg class="pulse-line" :class="`pulse-${state}`" viewBox="0 0 240 52" role="img" :aria-label="labels[state]">
    <line class="pulse-baseline" x1="0" y1="26" x2="240" y2="26" />
    <g class="pulse-motion">
      <polyline :points="points" />
    </g>
  </svg>
</template>
