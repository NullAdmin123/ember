import { defineComponent, h } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import PaymentsView from './PaymentsView.vue'
import { getAllPayments, getPlanGroups, getPlans } from '@/api/admin'
import { resolvePayment } from '@/api/entitlements'
import type { Payment } from '@/types/api'

vi.mock('@/api/admin', () => ({
  getAllPayments: vi.fn(),
  getPlanGroups: vi.fn(),
  getPlans: vi.fn(),
}))

vi.mock('@/api/entitlements', () => ({
  resolvePayment: vi.fn(),
}))

vi.mock('vue-router', () => ({
  useRoute: () => ({
    query: {},
  }),
  useRouter: () => ({
    replace: vi.fn(),
  }),
}))

vi.mock('element-plus', () => ({
  ElMessage: {
    success: vi.fn(),
    warning: vi.fn(),
  },
}))

const passthroughStub = defineComponent({
  setup(_, { slots }) {
    return () => h('div', [slots.default?.(), slots.actions?.(), slots.pagination?.()])
  },
})

const dialogStub = defineComponent({
  props: {
    modelValue: { type: Boolean, default: false },
    title: { type: String, default: '' },
  },
  emits: ['update:modelValue'],
  setup(props, { slots }) {
    return () =>
      props.modelValue
        ? h('div', { 'data-test': 'review-dialog' }, [
            h('div', { 'data-test': 'dialog-body' }, slots.default?.()),
            h('div', { 'data-test': 'dialog-footer' }, slots.footer?.()),
          ])
        : null
  },
})

describe('PaymentsView', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.mocked(getAllPayments).mockResolvedValue({
      data: [],
      total: 0,
      page: 1,
      pageSize: 20,
      totalPages: 0,
    } as never)
    vi.mocked(getPlans).mockResolvedValue({
      data: [],
      total: 0,
      page: 1,
      pageSize: 100,
      totalPages: 0,
    } as never)
    vi.mocked(getPlanGroups).mockResolvedValue({
      data: [],
    } as never)
  })

  it('处理已付款订单弹窗提供取消按钮并可关闭弹窗', async () => {
    const wrapper = mount(PaymentsView, {
      global: {
        stubs: {
          EmberFormDialog: dialogStub,
          EmberTableCard: passthroughStub,
          EmberPageHeaderCard: passthroughStub,
          EmberFilterPanel: passthroughStub,
          EmberSearchInput: passthroughStub,
          EmberSelectField: passthroughStub,
          PlanBenefitsEditor: passthroughStub,
          'el-form': passthroughStub,
          'el-form-item': passthroughStub,
          'el-select': passthroughStub,
          'el-option': true,
          'el-input': true,
          'el-table-column': true,
          'el-pagination': true,
          'el-tag': passthroughStub,
          'el-icon': passthroughStub,
        },
      },
    })
    await flushPromises()

    const vm = wrapper.vm as unknown as {
      openReview: (payment: Payment) => Promise<void>
      reviewPayment: Payment | null
    }

    await vm.openReview({
      id: 'pay_1',
      userId: 'u_1',
      planId: 'plan_1',
      amount: 1000,
      currency: 'usd',
      days: 30,
      status: 'paid_review',
      stripeSessionId: 'cs_test_1',
      createdAt: '2026-01-01T00:00:00Z',
      updatedAt: '2026-01-01T00:00:00Z',
    })
    await flushPromises()

    expect(wrapper.find('[data-test="review-dialog"]').exists()).toBe(true)
    const cancelBtn = wrapper
      .findAll('[data-test="dialog-footer"] button')
      .find((btn) => btn.text().includes('取消'))
    expect(cancelBtn, '应提供取消按钮').toBeTruthy()

    await cancelBtn!.trigger('click')
    await flushPromises()

    expect(vm.reviewPayment).toBeNull()
    expect(resolvePayment).not.toHaveBeenCalled()
  })
})
