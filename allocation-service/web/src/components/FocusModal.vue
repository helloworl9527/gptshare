<script setup>
import { nextTick, onBeforeUnmount, onMounted, ref } from 'vue'

defineProps({ title: { type: String, required: true } })
const emit = defineEmits(['close'])
const dialog = ref()
let previousFocus

function focusable() {
  return [...dialog.value.querySelectorAll('button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])')]
    .filter((node) => !node.disabled && node.getAttribute('aria-hidden') !== 'true' && node.type !== 'hidden')
}

function onKeydown(event) {
  if (event.key === 'Escape') {
    emit('close')
    return
  }
  if (event.key !== 'Tab') return
  const nodes = focusable()
  if (nodes.length === 0) return
  const first = nodes[0]
  const last = nodes[nodes.length - 1]
  if (event.shiftKey && document.activeElement === first) {
    event.preventDefault()
    last.focus()
  } else if (!event.shiftKey && document.activeElement === last) {
    event.preventDefault()
    first.focus()
  }
}

onMounted(async () => {
  previousFocus = document.activeElement
  await nextTick()
  focusable()[0]?.focus()
  document.addEventListener('keydown', onKeydown)
})

onBeforeUnmount(() => {
  document.removeEventListener('keydown', onKeydown)
  previousFocus?.focus?.()
})
</script>

<template>
  <div class="modal-backdrop" role="presentation" @mousedown.self="$emit('close')">
    <section ref="dialog" class="confirm-dialog" role="dialog" aria-modal="true" :aria-labelledby="`${title}-title`">
      <header class="dialog-head">
        <h2 :id="`${title}-title`">
          {{ title }}
        </h2>
        <button class="icon-button" type="button" aria-label="关闭弹窗" @click="$emit('close')">
          ×
        </button>
      </header>
      <slot />
    </section>
  </div>
</template>
