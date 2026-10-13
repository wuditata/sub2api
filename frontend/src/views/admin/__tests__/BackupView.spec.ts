import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import BackupView from '../BackupView.vue'

const {
  getS3Config,
  updateS3Config,
  getImageStorageConfig,
  getSchedule,
  updateSchedule,
  deleteBackup,
  listBackups,
  getDownloadURL,
} = vi.hoisted(() => ({
  getS3Config: vi.fn(),
  updateS3Config: vi.fn(),
  getImageStorageConfig: vi.fn(),
  getSchedule: vi.fn(),
  updateSchedule: vi.fn(),
  deleteBackup: vi.fn(),
  listBackups: vi.fn(),
  getDownloadURL: vi.fn(),
}))

vi.mock('@/api', () => ({
  adminAPI: {
    backup: {
      getS3Config,
      updateS3Config,
      testS3Connection: vi.fn(),
      getImageStorageConfig,
      updateImageStorageConfig: vi.fn(),
      testImageStorageConnection: vi.fn(),
      getSchedule,
      updateSchedule,
      createBackup: vi.fn(),
      listBackups,
      getBackup: vi.fn(),
      deleteBackup,
      getDownloadURL,
      restoreBackup: vi.fn(),
    },
  },
}))

vi.mock('@/stores', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn(),
    showWarning: vi.fn(),
  }),
}))

vi.mock('@/composables/useStepUp', () => ({
  useStepUp: () => ({ run: (fn: () => unknown) => fn() }),
  isStepUpBlocked: () => false,
  isStepUpCancelled: () => false,
  stepUpBlockReason: () => '',
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) =>
      params?.index !== undefined ? `${key}:${params.index}` : params?.day !== undefined ? `${key}:${params.day}` : key,
  }),
}))

const baseRecord = (id: string, parts?: unknown[]) => ({
  id,
  status: 'completed',
  backup_type: 'postgres',
  file_name: `${id}.sql.gz`,
  s3_key: `backups/${id}.sql.gz`,
  parts,
  size_bytes: 10,
  triggered_by: 'manual',
  started_at: '2026-08-09T00:00:00Z',
})

const wrappers: ReturnType<typeof mount>[] = []

function mountBackupView() {
  const wrapper = mount(BackupView, {
    global: {
      stubs: {
        TotpStepUpDialog: true,
      },
    },
  })
  wrappers.push(wrapper)
  return wrapper
}

describe('admin BackupView', () => {
  beforeEach(() => {
    getS3Config.mockResolvedValue({})
    getImageStorageConfig.mockResolvedValue({ config: {}, secret_configured: false })
    getSchedule.mockResolvedValue({ enabled: false, cron_expr: '', retain_days: 14, retain_count: 10 })
    updateSchedule.mockReset().mockResolvedValue({})
    deleteBackup.mockReset().mockResolvedValue(undefined)
    listBackups.mockResolvedValue({ items: [] })
    getDownloadURL.mockReset()
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
  })

  afterEach(() => {
    wrappers.splice(0).forEach(wrapper => wrapper.unmount())
    vi.restoreAllMocks()
    document.body.innerHTML = ''
  })

  it.each(['backup', 'restore'])('does not resume %s polling after navigation during initial loading', async (operation) => {
    vi.useFakeTimers()
    let finish!: (value: object) => void
    listBackups.mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
    const wrapper = mountBackupView()
    try {
      await flushPromises()
      wrapper.unmount()
      finish({ items: [{ ...baseRecord('pending'),
        status: operation === 'backup' ? 'running' : 'completed',
        restore_status: operation === 'restore' ? 'running' : undefined,
      }] })
      await flushPromises()
      expect(vi.getTimerCount()).toBe(0)
    } finally {
      wrapper.unmount()
      vi.clearAllTimers()
      vi.useRealTimers()
    }
  })

  it('显示分卷数并在下载时列出每个分卷链接', async () => {
    listBackups.mockResolvedValue({
      items: [baseRecord('split', [{ index: 1 }, { index: 2 }, { index: 3 }])],
    })
    getDownloadURL.mockResolvedValue({
      parts: [
        { index: 1, size_bytes: 5, url: 'https://example.test/part-1' },
        { index: 2, size_bytes: 6, url: 'https://example.test/part-2' },
        { index: 3, size_bytes: 7, url: 'https://example.test/part-3' },
      ],
    })

    const wrapper = mountBackupView()
    await flushPromises()

    expect(wrapper.text()).toContain('3')
    const downloadButton = wrapper.findAll('button').find(button =>
      button.text().includes('admin.backup.actions.download'),
    )
    expect(downloadButton).toBeDefined()
    await downloadButton!.trigger('click')
    await flushPromises()

    expect(document.body.textContent).toContain('admin.backup.actions.partLabel:1')
    expect(document.body.textContent).toContain('admin.backup.actions.partLabel:3')
    expect(document.body.querySelector('a[href="https://example.test/part-2"]')).not.toBeNull()
  })

  it('旧单文件记录仍使用单个下载地址', async () => {
    listBackups.mockResolvedValue({ items: [baseRecord('legacy')] })
    getDownloadURL.mockResolvedValue({ url: 'https://example.test/legacy.sql.gz' })

    const wrapper = mountBackupView()
    await flushPromises()
    const downloadButton = wrapper.findAll('button').find(button =>
      button.text().includes('admin.backup.actions.download'),
    )
    await downloadButton!.trigger('click')
    await flushPromises()

    expect(getDownloadURL).toHaveBeenCalledWith('legacy')
    expect(document.body.textContent).not.toContain('admin.backup.actions.downloadParts')
  })

  it('运行中的备份不显示删除入口', async () => {
    listBackups.mockResolvedValue({
      items: [{ ...baseRecord('running'), status: 'running', progress: 'uploading' }],
    })

    const wrapper = mountBackupView()
    await flushPromises()

    expect(wrapper.find('tbody tr td:nth-child(5)').text()).toBe('-')
    expect(wrapper.findAll('button').some(button => button.text() === 'common.delete')).toBe(false)
  })

  it('兼容旧配置并保留 0 值，不自动启用月度归档', async () => {
    getSchedule.mockResolvedValue({ enabled: true, cron_expr: '0 4 * * *', retain_days: 0, retain_count: 0 })
    const wrapper = mountBackupView()
    await flushPromises()
    expect((wrapper.get('[data-testid="backup-retain-days"]').element as HTMLInputElement).value).toBe('0')
    expect((wrapper.get('[data-testid="backup-retain-count"]').element as HTMLInputElement).value).toBe('0')
    expect((wrapper.get('[data-testid="archive-enabled"]').element as HTMLInputElement).checked).toBe(false)
    await wrapper.get('[data-testid="backup-schedule"] .btn-primary').trigger('click')
    await flushPromises()
    expect(updateSchedule).toHaveBeenCalledWith(expect.objectContaining({ retain_days: 0, retain_count: 0, monthly_archive: expect.objectContaining({ enabled: false }) }))
  })

  it('多选日期，手填份数；永久保留隐藏数量并在取消后恢复', async () => {
    const wrapper = mountBackupView()
    await flushPromises()
    await wrapper.get('[data-testid="archive-enabled"]').setValue(true)
    await wrapper.get('#backup-archive-dates').trigger('click')
    await wrapper.get('#backup-archive-options input[value="15"]').setValue(true)
    await wrapper.get('#backup-archive-options input[value="last"]').setValue(true)
    expect(wrapper.get('#backup-archive-dates').attributes('aria-expanded')).toBe('true')
    expect(wrapper.find('[data-testid="archive-count"]').exists()).toBe(false)
    await wrapper.get('[data-testid="archive-forever"]').setValue(false)
    await wrapper.get('[data-testid="archive-count"]').setValue(17)
    await wrapper.get('[data-testid="archive-forever"]').setValue(true)
    expect(wrapper.find('[data-testid="archive-count"]').exists()).toBe(false)
    expect(wrapper.findComponent({ name: 'BackupArchiveSettings' }).text()).not.toContain('admin.backup.archive.copies')
    await wrapper.get('[data-testid="archive-forever"]').setValue(false)
    expect((wrapper.get('[data-testid="archive-count"]').element as HTMLInputElement).value).toBe('17')
    await wrapper.get('[data-testid="backup-schedule"] .btn-primary').trigger('click')
    await flushPromises()
    expect(updateSchedule).toHaveBeenLastCalledWith(expect.objectContaining({ monthly_archive: { enabled: true, days: [1, 15], include_month_end: true, retain_count: 17 } }))
    await wrapper.get('[data-testid="archive-forever"]').setValue(true)
    await wrapper.get('[data-testid="backup-schedule"] .btn-primary').trigger('click')
    expect(updateSchedule).toHaveBeenLastCalledWith(expect.objectContaining({ monthly_archive: expect.objectContaining({ retain_count: 0 }) }))
  })

  it('空日期和非法份数不可保存，输入 0 不会误选永久保留', async () => {
    const wrapper = mountBackupView()
    await flushPromises()
    await wrapper.get('[data-testid="archive-enabled"]').setValue(true)
    await wrapper.get('#backup-archive-dates').trigger('click')
    await wrapper.get('#backup-archive-options input[value="1"]').setValue(false)
    const save = wrapper.get('[data-testid="backup-schedule"] .btn-primary')
    expect(save.attributes('disabled')).toBeDefined()
    await wrapper.get('#backup-archive-options input[value="15"]').setValue(true)
    await wrapper.get('[data-testid="archive-forever"]').setValue(false)
    for (const value of ['0', '-1', '1.5', '']) {
      await wrapper.get('[data-testid="archive-count"]').setValue(value)
      expect(save.attributes('disabled')).toBeDefined()
      expect((wrapper.get('[data-testid="archive-forever"]').element as HTMLInputElement).checked).toBe(false)
    }
    await save.trigger('click')
    expect(updateSchedule).not.toHaveBeenCalled()
  })

  it('关闭月度归档后不提交隐藏的归档参数，重新启用时保留编辑值', async () => {
    getSchedule.mockResolvedValue({ enabled: true, cron_expr: '0 4 * * *', retain_days: 14, retain_count: 10,
      monthly_archive: { enabled: true, days: [1, 15], include_month_end: false, retain_count: 12 } })
    const wrapper = mountBackupView()
    await flushPromises()
    await wrapper.get('[data-testid="archive-count"]').setValue(1)
    await wrapper.get('[data-testid="archive-enabled"]').setValue(false)
    expect(wrapper.find('[data-testid="archive-count"]').exists()).toBe(false)
    const save = wrapper.get('[data-testid="backup-schedule"] .btn-primary')
    await save.trigger('click')
    await flushPromises()
    expect(updateSchedule).toHaveBeenLastCalledWith(expect.objectContaining({ monthly_archive: { enabled: false, days: [1, 15], include_month_end: false, retain_count: 12 } }))
    await wrapper.get('[data-testid="archive-enabled"]').setValue(true)
    expect((wrapper.get('[data-testid="archive-count"]').element as HTMLInputElement).value).toBe('1')
    // A hidden invalid count neither blocks saving nor reaches the request.
    await wrapper.get('[data-testid="archive-count"]').setValue('')
    expect(save.attributes('disabled')).toBeDefined()
    await wrapper.get('[data-testid="archive-enabled"]').setValue(false)
    expect(save.attributes('disabled')).toBeUndefined()
    await save.trigger('click')
    await flushPromises()
    expect(updateSchedule).toHaveBeenLastCalledWith(expect.objectContaining({ monthly_archive: { enabled: false, days: [1, 15], include_month_end: false, retain_count: 12 } }))
    await wrapper.get('[data-testid="archive-enabled"]').setValue(true)
    await wrapper.get('[data-testid="archive-count"]').setValue(1)
    await save.trigger('click')
    await flushPromises()
    expect(updateSchedule).toHaveBeenLastCalledWith(expect.objectContaining({ monthly_archive: { enabled: true, days: [1, 15], include_month_end: false, retain_count: 1 } }))
  })

  it('加载归档设置并支持键盘关闭日期选择器', async () => {
    getSchedule.mockResolvedValue({ enabled: true, cron_expr: '0 4 * * *', retain_days: 14, retain_count: 10,
      monthly_archive: { enabled: true, days: [1, 15], include_month_end: true, retain_count: 23 } })
    const wrapper = mountBackupView()
    await flushPromises()
    expect((wrapper.get('[data-testid="archive-count"]').element as HTMLInputElement).value).toBe('23')
    await wrapper.get('#backup-archive-dates').trigger('keydown', { key: 'ArrowDown' })
    await flushPromises()
    expect(wrapper.find('#backup-archive-options').exists()).toBe(true)
    expect((wrapper.get('#backup-archive-options input[value="15"]').element as HTMLInputElement).checked).toBe(true)
    await wrapper.get('#backup-archive-options').trigger('keydown', { key: 'Escape' })
    expect(wrapper.find('#backup-archive-options').exists()).toBe(false)
  })

  it('归档删除需要单独确认，有限归档不会显示永不过期', async () => {
    listBackups.mockResolvedValue({ items: [{ ...baseRecord('archived'), monthly_archive: { dates: ['2026-09-01', '2026-09-15'], retain_count: 12 } }] })
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false)
    const wrapper = mountBackupView()
    await flushPromises()
    expect(wrapper.text()).toContain('admin.backup.archive.badge')
    expect(wrapper.get('tbody tr td:nth-child(6)').text()).toBe('admin.backup.archive.retainLatest')
    const button = wrapper.findAll('button').find(button => button.text() === 'common.delete')!
    await button.trigger('click')
    expect(confirm).toHaveBeenCalledWith('admin.backup.archive.deleteConfirm')
    expect(deleteBackup).not.toHaveBeenCalled()
    confirm.mockReturnValue(true)
    await button.trigger('click')
    await flushPromises()
    expect(deleteBackup).toHaveBeenCalledWith('archived', true)
  })

  it('默认渲染 S3 表单；旧配置无 storage_type 也按 S3 处理', async () => {
    getS3Config.mockResolvedValue({
      endpoint: 'https://r2.example.test', region: 'auto', bucket: 'b',
      access_key_id: 'ak', prefix: 'backups/', force_path_style: false,
    })
    const wrapper = mountBackupView()
    await flushPromises()

    const s3Radio = wrapper.find('input[type="radio"][value="s3"]')
    const localRadio = wrapper.find('input[type="radio"][value="local"]')
    expect((s3Radio.element as HTMLInputElement).checked).toBe(true)
    expect((localRadio.element as HTMLInputElement).checked).toBe(false)
    // S3 模式渲染备份存储的 S3 字段，不渲染本地目录提示。
    expect(wrapper.find('[data-testid="backup-s3-fields"]').exists()).toBe(true)
    expect(wrapper.text()).not.toContain('admin.backup.storage.localDirLabel')
  })

  it('切换到本地存储：隐藏 S3 表单、展示目录与持久化提示、禁用图片存储复用勾选', async () => {
    getS3Config.mockResolvedValue({
      storage_type: 'local', endpoint: '', region: '', bucket: '',
      access_key_id: '', prefix: '', force_path_style: false,
      local_dir: '/app/data/backups',
    })
    getImageStorageConfig.mockResolvedValue({
      config: { enabled: false, reuse_backup_s3: true, bucket: '', prefix: 'images/' },
      secret_configured: false,
    })
    const wrapper = mountBackupView()
    await flushPromises()

    // 本地模式：备份存储的 S3 字段隐藏，目录信息可见。
    expect(wrapper.find('[data-testid="backup-s3-fields"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('/app/data/backups')
    expect(wrapper.text()).toContain('admin.backup.storage.persistenceWarning')

    // 复用备份 S3 的勾选被强制关闭并禁用。
    const reuseCheckbox = wrapper.find('input[type="checkbox"][disabled]')
    expect((reuseCheckbox.element as HTMLInputElement).checked).toBe(false)
    expect(wrapper.text()).toContain('admin.backup.imageStorage.reuseUnavailableLocal')
    // 图片存储的 S3 凭证表单因复用被关闭而展开。
    expect(wrapper.findAll('input[placeholder="https://<account_id>.r2.cloudflarestorage.com"]').length).toBe(1)
  })

  it('本地模式下保存会把 storage_type=local 提交给原端点', async () => {
    updateS3Config.mockReset().mockResolvedValue({} as never)
    getS3Config.mockResolvedValue({
      storage_type: 'local', endpoint: '', region: '', bucket: '',
      access_key_id: '', prefix: '', force_path_style: false, local_dir: '/data/backups',
    })
    const wrapper = mountBackupView()
    await flushPromises()

    await wrapper.findAll('button').find(button => button.text().includes('common.save'))!.trigger('click')
    await flushPromises()

    expect(updateS3Config).toHaveBeenCalledWith(expect.objectContaining({ storage_type: 'local' }))
  })
})
