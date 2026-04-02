{ self }:
{ config, lib, pkgs, ... }:

let
  cfg = config.services.essence;
in
{
  options.services.essence = {
    enable = lib.mkEnableOption "Essence Quebec gas price map";

    port = lib.mkOption {
      type = lib.types.port;
      default = 8080;
      description = "Port the Essence web server listens on.";
    };

    openFirewall = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Whether to open the firewall for the Essence web server port.";
    };

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.system}.default;
      defaultText = lib.literalExpression "self.packages.\${pkgs.system}.default";
      description = "The Essence package to use.";
    };
  };

  config = lib.mkIf cfg.enable {
    systemd.services.essence = {
      description = "Essence Quebec - Gas price map";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];

      environment = {
        PORT = toString cfg.port;
      };

      serviceConfig = {
        ExecStart = lib.getExe cfg.package;
        Restart = "on-failure";
        RestartSec = 5;

        DynamicUser = true;

        # Hardening
        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectControlGroups = true;
        RestrictSUIDSGID = true;
        RestrictNamespaces = true;
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        RestrictRealtime = true;
        SystemCallFilter = [ "@system-service" "~@privileged" ];
      };
    };

    networking.firewall.allowedTCPPorts = lib.mkIf cfg.openFirewall [ cfg.port ];
  };
}
