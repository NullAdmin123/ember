import { defineComponent, h } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import UserPlaybackProfilesView from './UserPlaybackProfilesView.vue'
import { getUserPlaybackProfiles } from '@/api/admin'

vi.mock('@/api/admin', () => ({
  getUserPlaybackProfiles: vi.fn(),
}))

vi.mock('vue-router', () => ({
  useRoute: () => ({
    query: {},
  }),
  useRouter: () => ({
    push: vi.fn(),
  }),
}))

vi.mock('element-plus', () => ({
  ElMessage: {
    warning: vi.fn(),
  },
}))

const passthroughStub = defineComponent({
  setup(_, { slots }) {
    return () => h('div', [slots.default?.(), slots.actions?.(), slots.pagination?.()])
  },
})

const paginationStub = defineComponent({
  name: 'ElPagination',
  props: {
    currentPage: { type: Number, default: 1 },
    pageSize: { type: Number, default: 20 },
  },
  emits: ['update:currentPage', 'update:pageSize', 'current-change', 'size-change'],
  setup(_, { emit }) {
    return () =>
      h(
        'button',
        {
          'data-test': 'size-change-trigger',
          onClick: () => {
            emit('update:pageSize', 50)
            emit('size-change', 50)
          },
        },
        'change-size',
      )
  },
})

describe('UserPlaybackProfilesView', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.mocked(getUserPlaybackProfiles).mockResolvedValue({
      data: [],
      total: 120,
      page: 1,
      pageSize: 20,
      summary: {
        userCount: 0,
        totalPlayCount: 0,
        totalPlayDuration: 0,
        totalPlayDurationFormatted: '0m',
      },
    } as never)
  })

  it('切换每页条数时将页码重置为第 1 页再请求数据', async () => {
    const wrapper = mount(UserPlaybackProfilesView, {
      global: {
        stubs: {
          EmberMetricCard: passthroughStub,
          EmberTableCard: passthroughStub,
          EmberDateRangeField: passthroughStub,
          EmberSearchInput: passthroughStub,
          EmberFilterPanel: passthroughStub,
          EmberPageHeaderCard: passthroughStub,
          EmberSegmentTabs: passthroughStub,
          'el-pagination': paginationStub,
          'el-table-column': true,
          'el-icon': passthroughStub,
        },
      },
    })
    await flushPromises()

    const vm = wrapper.vm as unknown as {
      queryParams: { page: number; pageSize: number }
    }
    vm.queryParams.page = 3

    await wrapper.find('[data-test="size-change-trigger"]').trigger('click')
    await flushPromises()

    expect(vm.queryParams.page).toBe(1)
    expect(getUserPlaybackProfiles).toHaveBeenLastCalledWith(
      expect.objectContaining({
        page: 1,
        pageSize: 50,
      }),
    )
  })
})
