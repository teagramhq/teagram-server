function assessArtifactResponses(responses, origin, privateCSP) {
  const discoveryResponses = responses.filter((response) => {
    let parsed;
    try { parsed = new URL(response.url); } catch { return false; }
    return parsed.origin === origin && parsed.pathname === '/.well-known/teagramd/client';
  });
  const failedDiscoveryResponses = discoveryResponses.filter((response) => response.status !== 200);
  const artifactResponses = responses.filter((response) => {
    let parsed;
    try { parsed = new URL(response.url); } catch { return true; }
    return !(parsed.origin === origin && (
      parsed.pathname === '/.well-known/teagramd/client' ||
      parsed.pathname === '/teagram-config.json' && response.status === 404
    ));
  });
  const artifactResponsesWithPrivateCSP = artifactResponses.filter((response) =>
    response.content_security_policy === privateCSP
  ).length;
  const failedArtifactResponses = artifactResponses.filter((response) => response.status !== 200);
  const missingPrivateCSPResponses = artifactResponses.filter((response) =>
    response.content_security_policy !== privateCSP
  );
  const loadedScripts = artifactResponses.some((response) =>
    /\.m?js$/i.test(new URL(response.url).pathname)
  );
  const loadedStyles = artifactResponses.some((response) =>
    /\.css$/i.test(new URL(response.url).pathname)
  );

  return {
    artifactResponses,
    artifactResponsesWithPrivateCSP,
    failedDiscoveryResponses,
    failedArtifactResponses,
    missingPrivateCSPResponses,
    loadedScripts,
    loadedStyles,
    passed: artifactResponses.length >= 4 &&
      artifactResponsesWithPrivateCSP === artifactResponses.length &&
      failedDiscoveryResponses.length === 0 && failedArtifactResponses.length === 0 && loadedScripts && loadedStyles,
  };
}

module.exports = { assessArtifactResponses };
