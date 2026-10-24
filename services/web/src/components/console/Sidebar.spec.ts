import { defineComponent, h, reactive } from 'vue'
import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import Sidebar from './Sidebar.vue'

const authStoreState = reactive({
  role: 'user' as 'user' | 'admin',
  isAdmin: false,
})

vi.mock('@/store/auth', () => ({
  useAuthStore: vi.fn(() => ({
    get role() {
      return authStoreState.role
    },
    get isAdmin() {
      return authStoreState.isAdmin
    },
  })),
}))

vi.mock('vue-router', () => ({
  useRoute: () => ({
    path: '/console/dashboard',
  }),
}))

const routerLinkStub = defineComponent({
  name: 'RouterLink',
  props: {
    to: { type: String, required: true },
  },
  setup(props, { slots }) {
    return () => h('a', { href: props.to, 'data-to': props.to }, slots.default?.())
  },
})

describe('Sidebar', () => {
  it('普通用户显示续费中心，管理员侧边栏不展示个人续费中心', () => {
    authStoreState.role = 'user'
    authStoreState.isAdmin = false
    const userWrapper = mount(Sidebar, {
      global: {
        stubs: {
          'router-link': routerLinkStub,
          'el-icon': true,
          ProjectSourceLink: true,
        },
      },
    })
    expect(userWrapper.find('[data-to="/console/renewal"]').exists()).toBe(true)

    authStoreState.role = 'admin'
    authStoreState.isAdmin = true
    const adminWrapper = mount(Sidebar, {
      global: {
        stubs: {
          'router-link': routerLinkStub,
          'el-icon': true,
          ProjectSourceLink: true,
        },
      },
    })
    expect(adminWrapper.find('[data-to="/console/renewal"]').exists()).toBe(false)
    expect(adminWrapper.find('[data-to="/console/billing"]').exists()).toBe(true)
  })
})
