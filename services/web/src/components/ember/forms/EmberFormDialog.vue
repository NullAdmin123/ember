<script setup lang="ts">
import { computed } from 'vue'

defineOptions({
  inheritAttrs: false
})

const props = withDefaults(defineProps<{
  modelValue: boolean
  title: string
  width?: string
}>(), {
  width: '520px'
})

const emit = defineEmits<{
  'update:modelValue': [value: boolean]
}>()

/**
 * 将固定像素宽度转换为带视口边距上限的响应式表达式，避免窄屏移动端弹窗溢出屏幕；
 * 已显式声明 min()/clamp()/calc() 或百分比宽度时保持原值不变。
 */
const normalizedWidth = computed(() => {
  const raw = props.width.trim()
  if (/^\d+(?:\.\d+)?px$/i.test(raw)) {
    return `min(${raw}, calc(100vw - 2rem))`
  }
  return raw
})
</script>

<template>
  <el-dialog
    :model-value="props.modelValue"
    :title="props.title"
    :width="normalizedWidth"
    align-center
    append-to-body
    class="ember-form-dialog"
    v-bind="$attrs"
    @update:model-value="emit('update:modelValue', $event)"
  >
    <slot />

    <template v-if="$slots.footer" #footer>
      <slot name="footer" />
    </template>
  </el-dialog>
</template>
