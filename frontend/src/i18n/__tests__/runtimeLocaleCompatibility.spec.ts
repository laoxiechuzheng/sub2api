import { mount } from '@vue/test-utils'
import { defineComponent, nextTick } from 'vue'
import { useI18n } from 'vue-i18n'
import { afterEach, describe, expect, it } from 'vitest'

import i18n, { getLocale, initI18n, loadLocaleMessages } from '../index'

const initialLocale = i18n.global.locale.value
const initialDocumentLanguage = document.documentElement.lang

afterEach(() => {
  i18n.global.locale.value = initialLocale
  document.documentElement.lang = initialDocumentLanguage
})

describe('application locale runtime compatibility', () => {
  it('compiles lazily loaded messages and interpolates both application languages', async () => {
    await loadLocaleMessages('en')
    await loadLocaleMessages('zh')

    i18n.global.locale.value = 'en'
    expect(i18n.global.t('common.time.daysAgo', { n: 2 })).toBe('2d ago')
    expect(i18n.global.te('common.time.daysAgo')).toBe(true)
    expect(i18n.global.te('common.time.unknown-key')).toBe(false)

    i18n.global.locale.value = 'zh'
    await initI18n()
    expect(getLocale()).toBe('zh')
    expect(document.documentElement.lang).toBe('zh')
    expect(i18n.global.t('common.time.daysAgo', { n: 2 })).toBe('2天前')
  })

  it('keeps Composition API, injected $t and named component interpolation reactive', async () => {
    await loadLocaleMessages('en')
    await loadLocaleMessages('zh')
    i18n.global.locale.value = 'en'

    const component = defineComponent({
      setup: () => ({ t: useI18n().t }),
      template: `<div>
        <p>{{ t('common.time.daysAgo', { n: 2 }) }}</p>
        <p>{{ $t('common.time.never') }}</p>
        <i18n-t keypath="batchImage.pagination.pageNumber" scope="global" tag="p">
          <template #page><strong>3</strong></template>
        </i18n-t>
      </div>`
    })
    const wrapper = mount(component, { global: { plugins: [i18n] } })
    try {
      expect(wrapper.findAll('p').map(node => node.text())).toEqual(['2d ago', 'Never', 'Page 3'])
      expect(wrapper.find('strong').text()).toBe('3')

      i18n.global.locale.value = 'zh'
      await nextTick()
      expect(wrapper.findAll('p').map(node => node.text())).toEqual(['2天前', '从未', '第 3 页'])
      expect(wrapper.find('strong').text()).toBe('3')
    } finally {
      wrapper.unmount()
    }
  })
})
