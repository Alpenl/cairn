<script setup lang="ts">
import { onUnmounted } from 'vue'
import { NConfigProvider } from 'naive-ui'
import { useSystemTheme } from '@/capture/useSystemTheme'
import { useWebTagSettings } from '@/composables/useSettings'
import { useExtensionCapabilities } from '@/composables/useCapabilities'
import CaptureForm from './components/CaptureForm.vue'
import SubscriptionDiscovery from './components/SubscriptionDiscovery.vue'

const theme = useSystemTheme()
const { settings, ready, dispose } = useWebTagSettings()
const { policy } = useExtensionCapabilities(settings, ready)
onUnmounted(dispose)
</script>

<template>
  <NConfigProvider :theme="theme">
    <CaptureForm :site-enabled="policy.site">
      <template #after-page>
        <SubscriptionDiscovery />
      </template>
    </CaptureForm>
  </NConfigProvider>
</template>
