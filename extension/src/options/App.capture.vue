<script setup lang="ts">
import { NConfigProvider } from 'naive-ui'
import { useSystemTheme } from '@/capture/useSystemTheme'
import BackendSettings from './components/BackendSettings.vue'

const theme = useSystemTheme()
</script>

<template>
  <NConfigProvider :theme="theme">
    <main class="capture-options">
      <BackendSettings />
    </main>
  </NConfigProvider>
</template>

<style scoped>
.capture-options {
  width: min(100%, 860px);
  margin: 0 auto;
  padding: 28px 24px 48px;
}

@media (max-width: 640px) {
  .capture-options {
    padding: 20px 14px 36px;
  }
}
</style>
