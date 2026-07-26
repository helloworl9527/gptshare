<script setup>
defineProps({ items: { type: Array, required: true } })
</script>

<template>
  <section class="vitals-panel" aria-label="账号体征汇总">
    <article v-for="item in items" :key="item.key" class="kpi-card" :class="`kpi-${item.tone}`">
      <div class="kpi-head">
        <span>{{ item.label }}</span><span class="kpi-dot" aria-hidden="true" />
      </div>
      <strong>{{ item.value }}</strong>
      <p>{{ item.hint }}</p>
    </article>
  </section>
</template>
