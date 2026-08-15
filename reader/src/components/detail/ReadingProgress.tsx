import { Icon } from '../Icon'

function clampProgress(value: number): number {
  return Math.min(100, Math.max(0, Number.isFinite(value) ? value : 0))
}

export function ReadingProgressStrip({ progress }: { progress: number }) {
  return <span className="read-progress" style={{ width: `${clampProgress(progress)}%` }} />
}

export interface ReadingProgressSummaryProps {
  progress: number
  readMinutes: number
}

export function ReadingProgressSummary({ progress, readMinutes }: ReadingProgressSummaryProps) {
  const percentage = clampProgress(progress)
  const roundedPercentage = Math.round(percentage)
  const readableMinutes = Number.isFinite(readMinutes) ? Math.max(0, readMinutes) : 0
  const remainingMinutes = Math.max(
    0,
    Math.ceil(readableMinutes * (1 - percentage / 100)),
  )

  return (
    <section className="reader-rail-section reader-rail-progress" aria-labelledby="reader-rail-progress-title">
      <h2 id="reader-rail-progress-title" className="reader-rail-title">
        <Icon name="clock" size={13} /> 阅读进度
      </h2>
      <div
        className="reader-rail-progress-track"
        role="progressbar"
        aria-label="阅读进度"
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={roundedPercentage}
      >
        <span style={{ width: `${percentage}%` }} />
      </div>
      <p className="reader-rail-progress-copy">
        {roundedPercentage >= 100
          ? '已读完'
          : `已读 ${roundedPercentage}% · 剩 ${remainingMinutes} 分钟`}
      </p>
    </section>
  )
}
