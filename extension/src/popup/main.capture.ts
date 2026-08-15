import '../styles/capture.css'
import { createApp } from 'vue'
import { captureI18n } from '@/capture/i18n'
import App from './App.capture.vue'
import pkg from '../../package.json'

window.appVersion = pkg.version

createApp(App).use(captureI18n).mount('#app')
