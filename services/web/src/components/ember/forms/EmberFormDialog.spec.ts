import { defineComponent, h } from 'vue'
import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'

import EmberFormDialog from './EmberFormDialog.vue'

const elDialogStub = defineComponent({
  name: 'ElDialog',
  props: {
    modelValue: { type: Boolean, default: false },
    title: { type: String, default: '' },
    width: { type: String, default: '' },
  },
  setup(props, { slots }) {
    return () =>
      h(
        'div',
        {
          'data-test': 'el-dialog',
          'data-width': props.width,
        },
        [
          h('div', { 'data-test': 'body' }, slots.default?.()),
          h('div', { 'data-test': 'footer' }, slots.footer?.()),
        ],
      )
  },
})

describe('EmberFormDialog', () => {
  it('将固定像素宽度转换为移动端安全宽度表达式', () => {
    const wrapper = mount(EmberFormDialog, {
      props: {
        modelValue: true,
        title: '编辑用户',
        width: '680px',
      },
      global: {
        stubs: {
          'el-dialog': elDialogStub,
        },
      },
    })

    expect(wrapper.find('[data-test="el-dialog"]').attributes('data-width')).toBe(
      'min(680px, calc(100vw - 2rem))',
    )
  })

  it('保留已声明的响应式宽度表达式', () => {
    const wrapper = mount(EmberFormDialog, {
      props: {
        modelValue: true,
        title: '确认订阅',
        width: 'min(780px, calc(100vw - 2rem))',
      },
      global: {
        stubs: {
          'el-dialog': elDialogStub,
        },
      },
    })

    expect(wrapper.find('[data-test="el-dialog"]').attributes('data-width')).toBe(
      'min(780px, calc(100vw - 2rem))',
    )
  })
})
