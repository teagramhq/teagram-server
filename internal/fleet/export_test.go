package fleet

import (
	"context"
	"log/slog"
)

// PublishForTest runs one publication without starting the background ticker.
func PublishForTest(ctx context.Context, publisher *Publisher) error {
	return publisher.publish(ctx)
}

// SetPublisherLoggerForTest replaces the publisher logger for an assertion.
func SetPublisherLoggerForTest(publisher *Publisher, logger *slog.Logger) {
	publisher.logger = logger
}

// SafeBuildVersionForTest returns the version sanitizer's result.
func SafeBuildVersionForTest(version string) string {
	return safeBuildVersion(version)
}

// LogPublicationFailureForTest runs the sanitized failure logger.
func LogPublicationFailureForTest(publisher *Publisher, err error) {
	publisher.logPublicationFailure(err)
}
