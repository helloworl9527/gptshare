<script setup>
defineProps({
  type: { type: String, default: 'empty' },
  title: { type: String, required: true },
  message: { type: String, required: true },
  action: { type: String, default: '' },
})
defineEmits(['action'])
</script>

<template>
  <section class="state-panel" :class="`${type}-panel`" :role="type === 'error' ? 'alert' : 'status'">
    <span class="state-icon" aria-hidden="true">{{ type === 'error' ? '!' : '·' }}</span>
    <h3>{{ title }}</h3>
    <p>{{ message }}</p>
    <button v-if="action" type="button" @click="$emit('action')">
      {{ action }}
    </button>
  </section>
</template>
