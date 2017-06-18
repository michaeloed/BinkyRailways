﻿using System.Collections.Generic;
using System.Xml.Serialization;

namespace BinkyRailways.Core.Model.Impl
{
    /// <summary>
    /// MQTT command station
    /// </summary>
    [XmlRoot]
    public sealed class MqttCommandStation : CommandStation, IMqttCommandStation
    {
        private readonly Property<string> hostName;

        /// <summary>
        /// Default ctor
        /// </summary>
        public MqttCommandStation()
        {
            hostName = new Property<string>(this, "mqtt");
        }

        /// <summary>
        /// What types of addresses does this command station support?
        /// </summary>
        public override IEnumerable<AddressType> GetSupportedAddressTypes(IAddressEntity entity)
        {
            if (entity is ILoc)
            {
            }
            else
            {
                yield return AddressType.Mqtt;
            }
        }

        /// <summary>
        /// What types of addresses does this command station support?
        /// </summary>
        public override IEnumerable<AddressType> GetSupportedAddressTypes()
        {
            yield return AddressType.Mqtt;
        }

        /// <summary>
        /// Network hostname of the command station
        /// </summary>
        public string HostName
        {
            get { return hostName.Value; }
            set { hostName.Value = value ?? string.Empty; }
        }

        /// <summary>
        /// Accept a visit by the given visitor
        /// </summary>
        public override TReturn Accept<TReturn, TData>(EntityVisitor<TReturn, TData> visitor, TData data)
        {
            return visitor.Visit(this, data);
        }

        /// <summary>
        /// Validate the integrity of this entity.
        /// </summary>
        public override void Validate(IEntity validationRoot, ValidationResults results)
        {
            base.Validate(validationRoot, results);
            if (string.IsNullOrEmpty(HostName))
            {
                results.Warn(this, Strings.WarningNoHostnameSpecified);
            }
        }

        /// <summary>
        /// Human readable name of this type of entity.
        /// </summary>
        public override string TypeName
        {
            get { return Strings.TypeNameMqttCommandStation; }
        }
    }
}
