/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useForm, type Resolver } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import { z } from 'zod'

import { StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import {
  Card,
  CardAction,
  CardContent,
  CardHeader,
  CardTitle,
} from '@/components/ui/card'
import {
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
import { api } from '@/lib/api'
import dayjs from '@/lib/dayjs'

import {
  SettingsForm,
  SettingsSwitchContent,
  SettingsSwitchItem,
} from '../components/settings-form-layout'
import { SettingsPageFormActions } from '../components/settings-page-context'
import { SettingsSection } from '../components/settings-section'
import { useUpdateOption } from '../hooks/use-update-option'

const schema = z.object({
  enabled: z.boolean(),
  autoCheckinEnabled: z.boolean(),
  autoCheckinCron: z.string(),
  minQuota: z.coerce.number().int().min(0),
  maxQuota: z.coerce.number().int().min(0),
})

type Values = z.infer<typeof schema>

type AutoCheckinSummary = {
  total_users: number
  checked_in: number
  already_checked: number
  failed: number
  started_at: number
  finished_at: number
  duration_seconds: number
  trigger: string
}

type AutoCheckinStatus = {
  enabled: boolean
  running: boolean
  cron: string
  last_run_date: string
  last_run_at: number
  next_run_at: number
  last_summary?: AutoCheckinSummary
  last_error?: string
  scheduler_live: boolean
  is_master_node: boolean
}

type AutoCheckinStatusResponse = {
  success: boolean
  data: AutoCheckinStatus
  message?: string
}

type AutoCheckinTriggerResponse = {
  success: boolean
  data: AutoCheckinSummary
  message?: string
}

const AUTO_CHECKIN_STATUS_QUERY_KEY = ['auto-checkin-status'] as const

function formatAutoCheckinTime(timestamp: number | undefined, fallback: string) {
  if (!timestamp) return fallback
  return dayjs.unix(timestamp).format('YYYY-MM-DD HH:mm:ss')
}

export function CheckinSettingsSection({
  defaultValues,
}: {
  defaultValues: {
    enabled: boolean
    autoCheckinEnabled: boolean
    autoCheckinCron: string
    minQuota: number
    maxQuota: number
  }
}) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const updateOption = useUpdateOption()

  const form = useForm<Values>({
    resolver: zodResolver(schema) as unknown as Resolver<Values>,
    defaultValues: {
      enabled: defaultValues.enabled,
      autoCheckinEnabled: defaultValues.autoCheckinEnabled,
      autoCheckinCron: defaultValues.autoCheckinCron || '0 0 * * *',
      minQuota: defaultValues.minQuota,
      maxQuota: defaultValues.maxQuota,
    },
  })

  const { isDirty, isSubmitting } = form.formState
  const enabled = form.watch('enabled')
  const autoCheckinEnabled = form.watch('autoCheckinEnabled')

  const statusQuery = useQuery({
    queryKey: AUTO_CHECKIN_STATUS_QUERY_KEY,
    queryFn: async () => {
      const res = await api.get<AutoCheckinStatusResponse>(
        '/api/user/auto-checkin/status'
      )
      return res.data.data
    },
  })

  const triggerMutation = useMutation({
    mutationFn: async () => {
      const res = await api.post<AutoCheckinTriggerResponse>(
        '/api/user/auto-checkin/trigger',
        undefined,
        { skipBusinessError: true }
      )
      if (!res.data.success) {
        throw new Error(res.data.message || t('Request failed'))
      }
      return res.data.data
    },
    onSuccess: () => {
      toast.success(t('Triggered successfully'))
      queryClient.invalidateQueries({
        queryKey: AUTO_CHECKIN_STATUS_QUERY_KEY,
      })
    },
    onError: (error: Error) => {
      toast.error(error.message || t('Request failed'))
    },
  })

  async function onSubmit(values: Values) {
    const updates: Array<{ key: string; value: string }> = []

    if (values.enabled !== defaultValues.enabled) {
      updates.push({
        key: 'checkin_setting.enabled',
        value: String(values.enabled),
      })
    }

    if (values.autoCheckinEnabled !== defaultValues.autoCheckinEnabled) {
      updates.push({
        key: 'checkin_setting.auto_checkin_enabled',
        value: String(values.autoCheckinEnabled),
      })
    }

    if (values.autoCheckinCron !== defaultValues.autoCheckinCron) {
      updates.push({
        key: 'checkin_setting.auto_checkin_cron',
        value: values.autoCheckinCron,
      })
    }

    if (values.minQuota !== defaultValues.minQuota) {
      updates.push({
        key: 'checkin_setting.min_quota',
        value: String(values.minQuota),
      })
    }

    if (values.maxQuota !== defaultValues.maxQuota) {
      updates.push({
        key: 'checkin_setting.max_quota',
        value: String(values.maxQuota),
      })
    }

    if (updates.length === 0) {
      toast.info(t('No changes to save'))
      return
    }

    for (const update of updates) {
      await updateOption.mutateAsync(update)
    }

    form.reset(values)
  }

  return (
    <SettingsSection title={t('Check-in Settings')}>
      <Form {...form}>
        <SettingsForm onSubmit={form.handleSubmit(onSubmit)} autoComplete='off'>
          <SettingsPageFormActions
            onSave={form.handleSubmit(onSubmit)}
            isSaving={updateOption.isPending || isSubmitting}
            isSaveDisabled={!isDirty}
            saveLabel='Save check-in settings'
          />
          <FormField
            control={form.control}
            name='enabled'
            render={({ field }) => (
              <SettingsSwitchItem>
                <SettingsSwitchContent>
                  <FormLabel>{t('Enable check-in feature')}</FormLabel>
                  <FormDescription>
                    {t(
                      'Allow users to check in daily for random quota rewards'
                    )}
                  </FormDescription>
                </SettingsSwitchContent>
                <FormControl>
                  <Switch
                    checked={field.value}
                    onCheckedChange={field.onChange}
                    disabled={updateOption.isPending || isSubmitting}
                  />
                </FormControl>
              </SettingsSwitchItem>
            )}
          />

          {enabled && (
            <>
              <FormField
                control={form.control}
                name='autoCheckinEnabled'
                render={({ field }) => (
                  <SettingsSwitchItem>
                    <SettingsSwitchContent>
                      <FormLabel>{t('Enable auto check-in')}</FormLabel>
                    </SettingsSwitchContent>
                    <FormControl>
                      <Switch
                        checked={field.value}
                        onCheckedChange={field.onChange}
                        disabled={updateOption.isPending || isSubmitting}
                      />
                    </FormControl>
                  </SettingsSwitchItem>
                )}
              />

              {autoCheckinEnabled && (
                <>
                  <FormField
                    control={form.control}
                    name='autoCheckinCron'
                    render={({ field }) => (
                      <FormItem>
                        <FormLabel>
                          {t('Auto check-in cron expression')}
                        </FormLabel>
                        <FormControl>
                          <Input
                            placeholder='0 0 * * *'
                            disabled={updateOption.isPending || isSubmitting}
                            {...field}
                          />
                        </FormControl>
                        <FormDescription>
                          {t(
                            'Cron expression for auto check-in schedule (minute hour * * *)'
                          )}
                        </FormDescription>
                        <FormMessage />
                      </FormItem>
                    )}
                  />

                  <Card size='sm'>
                    <CardHeader>
                      <CardTitle>{t('Auto check-in status')}</CardTitle>
                      <CardAction>
                        <Button
                          type='button'
                          size='sm'
                          variant='outline'
                          disabled={triggerMutation.isPending}
                          onClick={() => triggerMutation.mutate()}
                        >
                          {triggerMutation.isPending
                            ? t('Running')
                            : t('Trigger Auto Check-in')}
                        </Button>
                      </CardAction>
                    </CardHeader>
                    <CardContent className='space-y-4'>
                      {statusQuery.isLoading ? (
                        <p className='text-muted-foreground text-sm'>
                          {t('Loading')}
                        </p>
                      ) : (
                        <>
                          <div className='grid gap-3 sm:grid-cols-2'>
                            <div className='space-y-1'>
                              <p className='text-muted-foreground text-xs'>
                                {t('Status')}
                              </p>
                              <div className='flex flex-wrap gap-2'>
                                <StatusBadge
                                  label={t(
                                    statusQuery.data?.enabled
                                      ? 'Enabled'
                                      : 'Disabled'
                                  )}
                                  variant={
                                    statusQuery.data?.enabled
                                      ? 'success'
                                      : 'neutral'
                                  }
                                  copyable={false}
                                />
                                <StatusBadge
                                  label={t(
                                    statusQuery.data?.running
                                      ? 'Running'
                                      : 'Idle'
                                  )}
                                  variant={
                                    statusQuery.data?.running
                                      ? 'warning'
                                      : 'neutral'
                                  }
                                  copyable={false}
                                />
                              </div>
                            </div>

                            <div className='space-y-1'>
                              <p className='text-muted-foreground text-xs'>
                                {t('Last run')}
                              </p>
                              <p className='text-sm'>
                                {formatAutoCheckinTime(
                                  statusQuery.data?.last_run_at,
                                  t('Never')
                                )}
                              </p>
                            </div>

                            <div className='space-y-1'>
                              <p className='text-muted-foreground text-xs'>
                                {t('Next run')}
                              </p>
                              <p className='text-sm'>
                                {formatAutoCheckinTime(
                                  statusQuery.data?.next_run_at,
                                  t('Not scheduled')
                                )}
                              </p>
                            </div>
                          </div>

                          {statusQuery.data?.last_summary ? (
                            <div className='space-y-2'>
                              <p className='text-muted-foreground text-xs'>
                                {t('Last result')}
                              </p>
                              <div className='grid gap-3 sm:grid-cols-4'>
                                <div>
                                  <p className='text-muted-foreground text-xs'>
                                    {t('Total users')}
                                  </p>
                                  <p className='text-sm font-medium'>
                                    {
                                      statusQuery.data.last_summary
                                        .total_users
                                    }
                                  </p>
                                </div>
                                <div>
                                  <p className='text-muted-foreground text-xs'>
                                    {t('Checked in')}
                                  </p>
                                  <p className='text-success text-sm font-medium'>
                                    {
                                      statusQuery.data.last_summary
                                        .checked_in
                                    }
                                  </p>
                                </div>
                                <div>
                                  <p className='text-muted-foreground text-xs'>
                                    {t('Already checked')}
                                  </p>
                                  <p className='text-sm font-medium'>
                                    {
                                      statusQuery.data.last_summary
                                        .already_checked
                                    }
                                  </p>
                                </div>
                                <div>
                                  <p className='text-muted-foreground text-xs'>
                                    {t('Failed')}
                                  </p>
                                  <p
                                    className={
                                      statusQuery.data.last_summary.failed > 0
                                        ? 'text-destructive text-sm font-medium'
                                        : 'text-success text-sm font-medium'
                                    }
                                  >
                                    {statusQuery.data.last_summary.failed}
                                  </p>
                                </div>
                              </div>
                            </div>
                          ) : (
                            <p className='text-muted-foreground text-sm'>
                              {t('No data')}
                            </p>
                          )}

                          {statusQuery.data?.last_error && (
                            <p className='text-destructive text-sm'>
                              {t('Error')}: {statusQuery.data.last_error}
                            </p>
                          )}
                        </>
                      )}
                    </CardContent>
                  </Card>
                </>
              )}

              <div className='grid gap-6 sm:grid-cols-2'>
                <FormField
                  control={form.control}
                  name='minQuota'
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('Minimum check-in quota')}</FormLabel>
                      <FormControl>
                        <Input
                          type='number'
                          min={0}
                          placeholder={t('1000')}
                          {...field}
                        />
                      </FormControl>
                      <FormDescription>
                        {t('Minimum quota amount awarded for check-in')}
                      </FormDescription>
                      <FormMessage />
                    </FormItem>
                  )}
                />

                <FormField
                  control={form.control}
                  name='maxQuota'
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('Maximum check-in quota')}</FormLabel>
                      <FormControl>
                        <Input
                          type='number'
                          min={0}
                          placeholder={t('10000')}
                          {...field}
                        />
                      </FormControl>
                      <FormDescription>
                        {t('Maximum quota amount awarded for check-in')}
                      </FormDescription>
                      <FormMessage />
                    </FormItem>
                  )}
                />
              </div>
            </>
          )}
        </SettingsForm>
      </Form>
    </SettingsSection>
  )
}
